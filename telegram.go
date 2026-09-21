package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	telegramSecretSchema      = 1
	telegramAggregationWindow = 15 * time.Second
	telegramRequestTimeout    = 10 * time.Second
)

var telegramRetryDelays = []time.Duration{0, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

type telegramSecretState struct {
	SchemaVersion int             `json:"schema_version"`
	BotToken      string          `json:"bot_token,omitempty"`
	Notified      map[string]bool `json:"notified,omitempty"`
}

type AuthFailureNotification struct {
	AuthID    string
	AuthIndex string
	Alias     string
	Reason    string
	Detected  time.Time
}

func (e AuthFailureNotification) key() string {
	if id := strings.TrimSpace(e.AuthID); id != "" {
		return "auth:" + id
	}
	if index := strings.TrimSpace(e.AuthIndex); index != "" {
		return "index:" + index
	}
	return ""
}

type telegramSender func(context.Context, string, string, string) error

type TelegramNotifier struct {
	mu          sync.Mutex
	store       *PluginState
	secretPath  string
	secret      telegramSecretState
	pending     map[string]AuthFailureNotification
	active      map[string]bool
	timer       *time.Timer
	aggregation time.Duration
	send        telegramSender
	wait        func(context.Context, time.Duration) error
	ctx         context.Context
	cancel      context.CancelFunc
	stopped     bool
}

func NewTelegramNotifier(store *PluginState, secretPath string) (*TelegramNotifier, error) {
	secret, err := loadTelegramSecretState(secretPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TelegramNotifier{
		store:       store,
		secretPath:  secretPath,
		secret:      secret,
		pending:     make(map[string]AuthFailureNotification),
		active:      make(map[string]bool),
		aggregation: telegramAggregationWindow,
		send:        sendTelegramMessage,
		wait:        waitTelegramRetry,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

func telegramSecretPath(legacyStatePath string) string {
	dir := filepath.Dir(legacyStatePath)
	return filepath.Join(dir, ".telegram-notifications.json")
}

func loadTelegramSecretState(path string) (telegramSecretState, error) {
	state := telegramSecretState{SchemaVersion: telegramSecretSchema, Notified: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return telegramSecretState{}, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return telegramSecretState{}, err
	}
	if state.SchemaVersion > telegramSecretSchema {
		return telegramSecretState{}, fmt.Errorf("telegram notification state is newer than supported: %d", state.SchemaVersion)
	}
	state.SchemaVersion = telegramSecretSchema
	state.BotToken = strings.TrimSpace(state.BotToken)
	if state.Notified == nil {
		state.Notified = map[string]bool{}
	}
	return state, nil
}

func saveTelegramSecretState(path string, state telegramSecretState) error {
	if path == "" {
		return errors.New("telegram secret path is required")
	}
	state.SchemaVersion = telegramSecretSchema
	if state.Notified == nil {
		state.Notified = map[string]bool{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFileAtomic(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func validateTelegramBotToken(token string) error {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(token) > 256 {
		return errors.New("Telegram Bot Token is invalid")
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != ':' && r != '_' && r != '-' {
			return errors.New("Telegram Bot Token is invalid")
		}
	}
	return nil
}

func validateTelegramChatID(chatID string) error {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return errors.New("Telegram Chat ID is required")
	}
	if len(chatID) > 256 || strings.ContainsAny(chatID, "\r\n\t ") {
		return errors.New("Telegram Chat ID is invalid")
	}
	return nil
}

func (n *TelegramNotifier) TokenConfigured() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.TrimSpace(n.secret.BotToken) != ""
}

func (n *TelegramNotifier) UpdateToken(token string, remove bool) error {
	if n == nil {
		return errors.New("Telegram notifier is unavailable")
	}
	token = strings.TrimSpace(token)
	if !remove && token == "" {
		return nil
	}
	if !remove {
		if err := validateTelegramBotToken(token); err != nil {
			return err
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	previous := n.secret.BotToken
	if remove {
		n.secret.BotToken = ""
	} else {
		n.secret.BotToken = token
	}
	if err := saveTelegramSecretState(n.secretPath, n.secret); err != nil {
		n.secret.BotToken = previous
		return err
	}
	if remove {
		n.pending = make(map[string]AuthFailureNotification)
		n.active = make(map[string]bool)
		if n.timer != nil {
			n.timer.Stop()
			n.timer = nil
		}
	}
	return nil
}

func (n *TelegramNotifier) Notify(event AuthFailureNotification) {
	if n == nil || n.store == nil {
		return
	}
	cfg := n.store.Config()
	if !cfg.TelegramNotificationsEnabled || validateTelegramChatID(cfg.TelegramChatID) != nil {
		return
	}
	if event.Detected.IsZero() {
		event.Detected = time.Now()
	}
	event.Alias = n.accountAlias(event)
	key := event.key()
	if key == "" {
		return
	}
	n.mu.Lock()
	if n.stopped || strings.TrimSpace(n.secret.BotToken) == "" || n.secret.Notified[key] || n.active[key] {
		n.mu.Unlock()
		return
	}
	n.active[key] = true
	n.pending[key] = event
	if n.timer == nil {
		n.timer = time.AfterFunc(n.aggregation, n.flush)
	}
	n.mu.Unlock()
	n.store.RecordLog("info", "notification.telegram_queued", "Telegram authentication alert queued", map[string]any{"auth_id": event.AuthID, "reason": event.Reason}, event.Detected)
}

func (n *TelegramNotifier) Recover(authID, authIndex string) {
	if n == nil {
		return
	}
	event := AuthFailureNotification{AuthID: authID, AuthIndex: authIndex}
	key := event.key()
	if key == "" {
		return
	}
	n.mu.Lock()
	delete(n.active, key)
	delete(n.pending, key)
	_, hadNotified := n.secret.Notified[key]
	delete(n.secret.Notified, key)
	if len(n.pending) == 0 && n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	var persistErr error
	if hadNotified {
		persistErr = saveTelegramSecretState(n.secretPath, n.secret)
	}
	n.mu.Unlock()
	if persistErr != nil && n.store != nil {
		n.store.RecordLog("error", "notification.telegram_state_failed", "Could not update Telegram notification state", map[string]any{"auth_id": authID}, time.Now())
	}
}

func (n *TelegramNotifier) accountAlias(event AuthFailureNotification) string {
	if strings.TrimSpace(event.Alias) != "" || n.store == nil {
		return strings.TrimSpace(event.Alias)
	}
	snapshot := n.store.Snapshot(event.Detected)
	for _, account := range snapshot.Accounts {
		if event.AuthID != "" && account.AuthID != event.AuthID {
			continue
		}
		if event.AuthID == "" && event.AuthIndex != "" && account.AuthIndex != event.AuthIndex {
			continue
		}
		if alias := strings.TrimSpace(account.Annotation.Alias); alias != "" {
			return alias
		}
		if alias := strings.TrimSpace(account.DisplayName); alias != "" {
			return alias
		}
		break
	}
	return ""
}

func (n *TelegramNotifier) flush() {
	n.mu.Lock()
	if n.timer != nil {
		n.timer.Stop()
	}
	if n.stopped {
		n.timer = nil
		n.mu.Unlock()
		return
	}
	events := make([]AuthFailureNotification, 0, len(n.pending))
	keys := make([]string, 0, len(n.pending))
	for key, event := range n.pending {
		if n.active[key] {
			keys = append(keys, key)
			events = append(events, event)
		}
	}
	n.pending = make(map[string]AuthFailureNotification)
	n.timer = nil
	token := n.secret.BotToken
	n.mu.Unlock()
	if len(events) == 0 {
		return
	}
	go n.deliver(keys, events, token)
}

func (n *TelegramNotifier) deliver(keys []string, events []AuthFailureNotification, token string) {
	cfg := n.store.Config()
	if !cfg.TelegramNotificationsEnabled || validateTelegramChatID(cfg.TelegramChatID) != nil {
		n.mu.Lock()
		for _, key := range keys {
			delete(n.active, key)
		}
		n.mu.Unlock()
		return
	}
	message := formatTelegramAuthFailureMessage(events, cfg.TelegramLanguage)
	var err error
	for _, delay := range telegramRetryDelays {
		if delay > 0 {
			if err = n.wait(n.ctx, delay); err != nil {
				return
			}
		}
		requestCtx, cancel := context.WithTimeout(n.ctx, telegramRequestTimeout)
		err = n.send(requestCtx, token, cfg.TelegramChatID, message)
		cancel()
		if err == nil {
			break
		}
	}
	now := time.Now()
	if err != nil {
		for _, key := range keys {
			n.mu.Lock()
			delete(n.active, key)
			n.mu.Unlock()
		}
		n.store.RecordLog("error", "notification.telegram_failed", "Telegram authentication alert delivery failed", map[string]any{"account_count": len(events)}, now)
		return
	}
	n.mu.Lock()
	for _, key := range keys {
		if n.active[key] {
			n.secret.Notified[key] = true
		}
	}
	persistErr := saveTelegramSecretState(n.secretPath, n.secret)
	n.mu.Unlock()
	if persistErr != nil {
		n.store.RecordLog("error", "notification.telegram_state_failed", "Telegram alert was sent but its deduplication state could not be saved", map[string]any{"account_count": len(events)}, now)
	}
	n.store.RecordLog("info", "notification.telegram_sent", "Telegram authentication alert sent", map[string]any{"account_count": len(events)}, now)
}

func (n *TelegramNotifier) SendTest(ctx context.Context) error {
	if n == nil || n.store == nil {
		return errors.New("Telegram notifier is unavailable")
	}
	cfg := n.store.Config()
	if !cfg.TelegramNotificationsEnabled {
		return errors.New("Telegram Disabled")
	}
	if err := validateTelegramChatID(cfg.TelegramChatID); err != nil {
		return err
	}
	n.mu.Lock()
	token := n.secret.BotToken
	n.mu.Unlock()
	if token == "" {
		return errors.New("Telegram Bot Token is not configured")
	}
	message := "✅ Codex Fleet Manager Telegram notifications are configured correctly."
	if cfg.TelegramLanguage == "zh-CN" {
		message = "✅ Codex Fleet Manager 的 Telegram 通知配置正确。"
	}
	requestCtx, cancel := context.WithTimeout(ctx, telegramRequestTimeout)
	defer cancel()
	if err := n.send(requestCtx, token, cfg.TelegramChatID, message); err != nil {
		return errors.New("Telegram test notification failed")
	}
	n.store.RecordLog("info", "notification.telegram_test_sent", "Telegram test notification sent", nil, time.Now())
	return nil
}

func (n *TelegramNotifier) Stop() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	n.stopped = true
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	n.cancel()
	n.mu.Unlock()
}

func formatTelegramAuthFailureMessage(events []AuthFailureNotification, language string) string {
	sort.Slice(events, func(i, j int) bool { return events[i].key() < events[j].key() })
	zh := language == "zh-CN"
	var b strings.Builder
	b.WriteString("⚠️ Codex Fleet Manager\n\n")
	if zh {
		fmt.Fprintf(&b, "%d 个账号需要重新登录：\n", len(events))
	} else if len(events) == 1 {
		b.WriteString("1 account requires re-login:\n")
	} else {
		fmt.Fprintf(&b, "%d accounts require re-login:\n", len(events))
	}
	latest := time.Time{}
	for _, event := range events {
		name := strings.TrimSpace(event.Alias)
		identifier := strings.TrimSpace(event.AuthID)
		if identifier == "" {
			identifier = strings.TrimSpace(event.AuthIndex)
		}
		if name == "" {
			name = identifier
		}
		if identifier != "" && identifier != name {
			fmt.Fprintf(&b, "\n• %s — %s — %s", name, identifier, event.Reason)
		} else {
			fmt.Fprintf(&b, "\n• %s — %s", name, event.Reason)
		}
		if event.Detected.After(latest) {
			latest = event.Detected
		}
	}
	if latest.IsZero() {
		latest = time.Now()
	}
	if zh {
		fmt.Fprintf(&b, "\n\n检测时间：%s", latest.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "\n\nDetected: %s", latest.Format(time.RFC3339))
	}
	return b.String()
}

func sendTelegramMessage(ctx context.Context, token, chatID, message string) error {
	if err := validateTelegramBotToken(token); err != nil {
		return err
	}
	if err := validateTelegramChatID(chatID); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"chat_id": chatID, "text": message})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("could not create Telegram request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("Telegram request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Telegram API returned HTTP %d", response.StatusCode)
	}
	return nil
}

func waitTelegramRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var (
	globalTelegramMu       sync.RWMutex
	globalTelegramNotifier *TelegramNotifier
)

func replaceGlobalTelegramNotifier(next *TelegramNotifier) {
	globalTelegramMu.Lock()
	previous := globalTelegramNotifier
	globalTelegramNotifier = next
	globalTelegramMu.Unlock()
	if previous != nil {
		previous.Stop()
	}
}

func currentTelegramNotifier() *TelegramNotifier {
	globalTelegramMu.RLock()
	defer globalTelegramMu.RUnlock()
	return globalTelegramNotifier
}

func notifyAuthenticationFailure(account AccountState, reason string, now time.Time) {
	if notifier := currentTelegramNotifier(); notifier != nil {
		notifier.Notify(AuthFailureNotification{AuthID: account.AuthID, AuthIndex: account.AuthIndex, Alias: account.Annotation.Alias, Reason: reason, Detected: now})
	}
}

func notifyAuthenticationRecovery(authID, authIndex string) {
	if notifier := currentTelegramNotifier(); notifier != nil {
		notifier.Recover(authID, authIndex)
	}
}
