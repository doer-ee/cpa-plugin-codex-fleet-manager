package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type telegramCapture struct {
	mu       sync.Mutex
	messages []string
	err      error
}

func (c *telegramCapture) send(_ context.Context, _, _, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, message)
	return c.err
}

func (c *telegramCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.messages...)
}

func newTelegramTestNotifier(t *testing.T, cfg Config) (*TelegramNotifier, *PluginState, *telegramCapture) {
	t.Helper()
	store := NewPluginState(cfg)
	notifier, err := NewTelegramNotifier(store, filepath.Join(t.TempDir(), "telegram.json"))
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}
	t.Cleanup(notifier.Stop)
	if err := notifier.UpdateToken("123456:test_token", false); err != nil {
		t.Fatalf("UpdateToken returned error: %v", err)
	}
	capture := &telegramCapture{}
	notifier.send = capture.send
	notifier.wait = func(context.Context, time.Duration) error { return nil }
	return notifier, store, capture
}

func TestTelegramNotifierAggregatesAndDeduplicatesAuthFailures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "-100123"
	notifier, store, capture := newTelegramTestNotifier(t, cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-a", Provider: "codex", Annotation: AccountAnnotation{Alias: "Primary"}})
	store.UpsertQuota(AccountState{AuthID: "auth-b", Provider: "codex", Annotation: AccountAnnotation{Alias: "Backup"}})
	now := time.Date(2026, 9, 20, 22, 15, 30, 0, time.UTC)
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now})
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now})
	notifier.Notify(AuthFailureNotification{AuthID: "auth-b", Reason: "invalid_grant", Detected: now.Add(time.Second)})
	notifier.flush()

	if !waitUntil(time.Second, func() bool { return len(capture.snapshot()) == 1 }) {
		t.Fatalf("messages = %#v, want one aggregated delivery", capture.snapshot())
	}
	message := capture.snapshot()[0]
	for _, want := range []string{"2 accounts require re-login", "Primary — auth-a — HTTP 401", "Backup — auth-b — invalid_grant"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message missing %q: %s", want, message)
		}
	}
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now.Add(time.Minute)})
	notifier.flush()
	time.Sleep(20 * time.Millisecond)
	if got := len(capture.snapshot()); got != 1 {
		t.Fatalf("duplicate notification deliveries = %d, want 1", got)
	}
}

func TestTelegramNotifierRecoveryRearmsAndPersistsDeduplication(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "123"
	notifier, _, capture := newTelegramTestNotifier(t, cfg)
	now := time.Now()
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now})
	notifier.flush()
	if !waitUntil(time.Second, func() bool {
		state, err := loadTelegramSecretState(notifier.secretPath)
		return err == nil && len(capture.snapshot()) == 1 && state.Notified["auth:auth-a"]
	}) {
		t.Fatal("first delivery did not complete")
	}
	loaded, err := loadTelegramSecretState(notifier.secretPath)
	if err != nil || !loaded.Notified["auth:auth-a"] {
		t.Fatalf("persisted state = %#v, err=%v", loaded, err)
	}
	restarted, err := NewTelegramNotifier(notifier.store, notifier.secretPath)
	if err != nil {
		t.Fatalf("restart notifier: %v", err)
	}
	restartCapture := &telegramCapture{}
	restarted.send = restartCapture.send
	restarted.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now.Add(30 * time.Minute)})
	restarted.flush()
	time.Sleep(20 * time.Millisecond)
	restarted.Stop()
	if len(restartCapture.snapshot()) != 0 {
		t.Fatalf("restart resent acknowledged incident: %#v", restartCapture.snapshot())
	}
	notifier.Recover("auth-a", "")
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: now.Add(time.Hour)})
	notifier.flush()
	if !waitUntil(time.Second, func() bool { return len(capture.snapshot()) == 2 }) {
		t.Fatalf("messages after recovery = %#v, want second delivery", capture.snapshot())
	}
}

func TestTelegramNotifierRequiresEnabledCompleteConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		token bool
	}{
		{name: "disabled", cfg: DefaultConfig(), token: true},
		{name: "missing chat", cfg: func() Config { c := DefaultConfig(); c.TelegramNotificationsEnabled = true; return c }(), token: true},
		{name: "missing token", cfg: func() Config {
			c := DefaultConfig()
			c.TelegramNotificationsEnabled = true
			c.TelegramChatID = "123"
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewPluginState(tt.cfg)
			notifier, err := NewTelegramNotifier(store, filepath.Join(t.TempDir(), "telegram.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer notifier.Stop()
			capture := &telegramCapture{}
			notifier.send = capture.send
			if tt.token {
				if err := notifier.UpdateToken("123456:test_token", false); err != nil {
					t.Fatal(err)
				}
			}
			notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: time.Now()})
			notifier.flush()
			time.Sleep(20 * time.Millisecond)
			if len(capture.snapshot()) != 0 {
				t.Fatalf("messages = %#v, want none", capture.snapshot())
			}
		})
	}
}

func TestTelegramDeliveryFailureDoesNotMutateSchedulerHealth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "123"
	notifier, store, capture := newTelegramTestNotifier(t, cfg)
	capture.err = errors.New("offline")
	account := AccountState{AuthID: "auth-a", Provider: "codex", LastSuccessAt: time.Now()}
	store.UpsertQuota(account)
	notifier.Notify(AuthFailureNotification{AuthID: "auth-a", Reason: "HTTP 401", Detected: time.Now()})
	notifier.flush()
	if !waitUntil(time.Second, func() bool {
		for _, entry := range store.Snapshot(time.Now()).Logs {
			if entry.Event == "notification.telegram_failed" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("failure log was not recorded")
	}
	got := accountByAuthID(t, store.Snapshot(time.Now()), "auth-a")
	if got.Refresh.AuthFailure || got.Circuit.FailureCount != 0 || got.TemporaryExhausted {
		t.Fatalf("Telegram failure changed scheduler state: %#v", got)
	}
}

func TestUsageFeedback401MarksAuthFailureAndNotifiesOnce(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "123"
	notifier, store, capture := newTelegramTestNotifier(t, cfg)
	previous := currentTelegramNotifier()
	replaceGlobalTelegramNotifier(notifier)
	t.Cleanup(func() { replaceGlobalTelegramNotifier(previous) })
	store.UpsertQuota(AccountState{AuthID: "auth-a", AuthIndex: "idx-a", Provider: "codex"})
	record := pluginapi.UsageRecord{Provider: "codex", AuthID: "auth-a", AuthIndex: "idx-a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 401}}
	if !HandleUsageFeedback(store, record, time.Now()) {
		t.Fatal("401 usage feedback was not handled")
	}
	if !HandleUsageFeedback(store, record, time.Now().Add(time.Second)) {
		t.Fatal("repeated 401 usage feedback was not handled")
	}
	notifier.flush()
	if !waitUntil(time.Second, func() bool { return len(capture.snapshot()) == 1 }) {
		t.Fatalf("messages = %#v, want one", capture.snapshot())
	}
	account := accountByAuthID(t, store.Snapshot(time.Now()), "auth-a")
	if !account.Refresh.AuthFailure || account.Refresh.LastFailureKind != RefreshFailureAuth {
		t.Fatalf("account refresh state = %#v", account.Refresh)
	}
}

func TestBackgroundRefreshAuthenticationFailuresNotify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "unauthorized", status: 401, body: `{"error":"unauthorized"}`, want: "HTTP 401"},
		{name: "invalid grant", status: 400, body: `{"error":"invalid_grant"}`, want: "invalid_grant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 22, 15, 30, 0, time.UTC)
			cfg := DefaultConfig()
			cfg.TelegramNotificationsEnabled = true
			cfg.TelegramChatID = "123"
			store := NewPluginState(cfg)
			notifier, err := NewTelegramNotifier(store, filepath.Join(t.TempDir(), "telegram.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := notifier.UpdateToken("123456:test_token", false); err != nil {
				t.Fatal(err)
			}
			capture := &telegramCapture{}
			notifier.send = capture.send
			notifier.wait = func(context.Context, time.Duration) error { return nil }
			previous := currentTelegramNotifier()
			replaceGlobalTelegramNotifier(notifier)
			t.Cleanup(func() { replaceGlobalTelegramNotifier(previous) })

			idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
			host := &fakeHostClient{
				authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
				authJSON: map[string]json.RawMessage{
					"idx-1": json.RawMessage(`{"access_token":"old","refresh_token":"refresh","id_token":"` + idToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`),
				},
				responseByURL: map[string]pluginapi.HTTPResponse{
					codexTokenEndpoint: {StatusCode: tt.status, Body: []byte(tt.body)},
				},
			}
			refresher := newAdmittedQuotaRefresherForTest(host, store, func() time.Time { return now })
			if err := refresher.RefreshOnce(); err != nil {
				t.Fatalf("RefreshOnce returned error: %v", err)
			}
			notifier.flush()
			if !waitUntil(time.Second, func() bool { return len(capture.snapshot()) == 1 }) {
				t.Fatalf("messages = %#v, want one", capture.snapshot())
			}
			if !strings.Contains(capture.snapshot()[0], tt.want) {
				t.Fatalf("message = %q, want reason %q", capture.snapshot()[0], tt.want)
			}
		})
	}
}

func TestTelegramTokenIsWriteOnlyAndExcludedFromExport(t *testing.T) {
	dir := t.TempDir()
	previousPath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousPath })
	cfg := DefaultConfig()
	store := NewPluginState(cfg)
	notifier, err := NewTelegramNotifier(store, telegramSecretPath(defaultStatePath()))
	if err != nil {
		t.Fatal(err)
	}
	previous := currentTelegramNotifier()
	replaceGlobalTelegramNotifier(notifier)
	t.Cleanup(func() { replaceGlobalTelegramNotifier(previous) })

	payload := SettingsFromConfig(cfg)
	payload.TelegramNotificationsEnabled = true
	payload.TelegramChatID = "123"
	payload.TelegramLanguage = "en"
	payload.TelegramBotToken = "123456:secret_sentinel"
	body, _ := json.Marshal(payload)
	response := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "PUT", Path: managementBasePath + "/settings", Body: body}, time.Now())
	if response.StatusCode != 200 {
		t.Fatalf("settings response = %d %s", response.StatusCode, response.Body)
	}
	for _, response := range []pluginapi.ManagementResponse{
		HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: managementBasePath + "/settings"}, time.Now()),
		HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: managementBasePath + "/export"}, time.Now()),
		HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: managementBasePath + "/status", Query: url.Values{"format": {"json"}}}, time.Now()),
		HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: managementBasePath + "/status"}, time.Now()),
	} {
		if strings.Contains(string(response.Body), "secret_sentinel") {
			t.Fatalf("management response leaked token: %s", response.Body)
		}
	}
	secretRaw, err := os.ReadFile(telegramSecretPath(defaultStatePath()))
	if err != nil || !strings.Contains(string(secretRaw), "secret_sentinel") {
		t.Fatalf("secret file missing token: err=%v body=%s", err, secretRaw)
	}
	info, err := os.Stat(telegramSecretPath(defaultStatePath()))
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("secret permissions = %v; want 0600", info.Mode().Perm())
	}
}

func TestRemovingTelegramTokenAlsoDisablesNotificationsAndClearsChatID(t *testing.T) {
	dir := t.TempDir()
	previousPath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousPath })

	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "123"
	store := NewPluginState(cfg)
	notifier, err := NewTelegramNotifier(store, telegramSecretPath(defaultStatePath()))
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.UpdateToken("123456:secret_sentinel", false); err != nil {
		t.Fatal(err)
	}
	previous := currentTelegramNotifier()
	replaceGlobalTelegramNotifier(notifier)
	t.Cleanup(func() { replaceGlobalTelegramNotifier(previous) })

	payload := SettingsFromConfig(cfg)
	payload.TelegramNotificationsEnabled = true
	payload.TelegramChatID = "123"
	payload.TelegramRemoveBotToken = true
	body, _ := json.Marshal(payload)
	response := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "PUT", Path: managementBasePath + "/settings", Body: body}, time.Now())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings response = %d %s", response.StatusCode, response.Body)
	}
	got := store.Config()
	if got.TelegramNotificationsEnabled || got.TelegramChatID != "" {
		t.Fatalf("Telegram settings were not cleared: enabled=%v chat_id=%q", got.TelegramNotificationsEnabled, got.TelegramChatID)
	}
	if notifier.TokenConfigured() {
		t.Fatal("Telegram Bot Token is still configured")
	}
}

func TestTelegramTestMessageUsesConfiguredLanguage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TelegramNotificationsEnabled = true
	cfg.TelegramChatID = "123"
	cfg.TelegramLanguage = "zh-CN"
	notifier, _, capture := newTelegramTestNotifier(t, cfg)
	if err := notifier.SendTest(context.Background()); err != nil {
		t.Fatalf("SendTest returned error: %v", err)
	}
	if got := capture.snapshot(); len(got) != 1 || !strings.Contains(got[0], "配置正确") {
		t.Fatalf("test messages = %#v", got)
	}
}

func TestTelegramTestMessageReportsDisabledBeforeCredentialErrors(t *testing.T) {
	cfg := DefaultConfig()
	notifier, _, _ := newTelegramTestNotifier(t, cfg)
	err := notifier.SendTest(context.Background())
	if err == nil || err.Error() != "Telegram Disabled" {
		t.Fatalf("SendTest error = %v, want Telegram Disabled", err)
	}
}

func TestTelegramManagementUIAndRouteAreRegistered(t *testing.T) {
	page := string(RenderStatusHTML(BuildStatusShellPayload(time.Now())))
	for _, want := range []string{`id="telegramEnabled"`, `id="telegramBotToken" type="password"`, `id="telegramChatID"`, `id="telegramTest"`, `Test and Save Telegram Settings`, `Telegram Disabled`, `Bot Token configured`, `Bot Token 已配置`, `telegramCredentialsReady`, `autoEnableTelegramNotifications`, `handleTelegramRemoveChange`, `if(removing||!enabled)`, `telegramBotToken').addEventListener('input',autoEnableTelegramNotifications)`, `telegramChatID').addEventListener('input',autoEnableTelegramNotifications)`, `telegramRemoveToken').addEventListener('change',handleTelegramRemoveChange)`} {
		if !strings.Contains(page, want) {
			t.Fatalf("management page missing %q", want)
		}
	}
	if strings.Contains(page, "function handleTelegramRemoveChange(){const remove=document.getElementById('telegramRemoveToken');if(!remove.checked){autoEnableTelegramNotifications();return}document.getElementById('telegramEnabled').checked=false") {
		t.Fatal("checking Telegram token removal still clears visible settings before save")
	}
	found := false
	for _, route := range RegisterManagement().Routes {
		if route.Method == "POST" && route.Path == managementBasePath+"/telegram/test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Telegram test route is not registered")
	}
}
