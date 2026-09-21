package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type PluginDiskState struct {
	Config   Config                       `json:"config"`
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty"`
}

// PersistentState is deliberately separate from PluginDiskState: the former
// contains only scheduler-control hashes and epochs, never host auth material.

var defaultStatePath = resolveDefaultStatePath

const stateDirectoryEnvironment = "CODEX_FLEET_MANAGER_STATE_DIR"

func resolveDefaultStatePath() string {
	if dir := strings.TrimSpace(os.Getenv(stateDirectoryEnvironment)); dir != "" {
		return filepath.Join(dir, "state.json")
	}
	for _, root := range persistentPluginRoots() {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return filepath.Join(root, "data", PluginID, "state.json")
		}
	}
	return legacyDefaultStatePath()
}

func persistentPluginRoots() []string {
	roots := []string{filepath.FromSlash("/CLIProxyAPI/plugins")}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		roots = append(roots, filepath.Join(cwd, "plugins"))
	}
	return roots
}

func legacyDefaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "CLIProxyAPI", PluginID, "state.json")
}

// migrateLegacyDefaultState copies plugin-owned state from the historical user
// config directory into the persistent plugin volume. The source is retained so
// a rollback to an older plugin can still use it. Existing destination files are
// never overwritten.
func migrateLegacyDefaultState() error {
	return migrateStateDirectory(filepath.Dir(legacyDefaultStatePath()), filepath.Dir(defaultStatePath()))
}

func migrateStateDirectory(sourceDir, destinationDir string) error {
	if sourceDir == "" || destinationDir == "" {
		return nil
	}
	sourceAbs, sourceErr := filepath.Abs(sourceDir)
	destinationAbs, destinationErr := filepath.Abs(destinationDir)
	if sourceErr == nil && destinationErr == nil && sourceAbs == destinationAbs {
		return nil
	}
	artifacts := []string{
		"state.json", "state.json.bak", "state.json.migrated",
		".user-data.json", ".user-data.json.bak",
		".runtime-state.json", ".runtime-state.json.bak",
		".telegram-notifications.json",
	}
	for _, name := range artifacts {
		source := filepath.Join(sourceDir, name)
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect legacy state %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		destination := filepath.Join(destinationDir, name)
		if _, err := os.Stat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect persistent state %s: %w", name, err)
		}
		if err := copyStateArtifact(source, destination); err != nil {
			return fmt.Errorf("migrate state %s: %w", name, err)
		}
	}
	return nil
}

func copyStateArtifact(source, destination string) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func LoadPluginDiskState(path string) (PluginDiskState, error) {
	state, _, err := loadPluginDiskState(path)
	return state, err
}

func loadPluginDiskState(path string) (PluginDiskState, bool, error) {
	state := PluginDiskState{Config: DefaultConfig()}
	if path == "" {
		return normalizePluginDiskState(state), false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalizePluginDiskState(state), false, nil
	}
	if err != nil {
		return PluginDiskState{}, false, err
	}
	if len(raw) == 0 {
		return normalizePluginDiskState(state), false, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return PluginDiskState{}, false, err
	}
	return normalizePluginDiskState(state), true, nil
}

func SavePluginDiskState(path string, state PluginDiskState) error {
	if path == "" {
		path = defaultStatePath()
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(normalizePluginDiskState(state), "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0600)
}

func normalizePluginDiskState(state PluginDiskState) PluginDiskState {
	cfg := state.Config
	if cfg.QuotaRefreshInterval <= 0 && cfg.StaleAfter <= 0 && cfg.MonthlyMode == "" {
		cfg = DefaultConfig()
	}
	cfg = NormalizeConfig(cfg)
	if _, err := validateQuotaEndpoint(cfg.QuotaEndpoint); err != nil {
		cfg.QuotaEndpoint = DefaultConfig().QuotaEndpoint
	}
	annotations := NormalizeAnnotationState(AnnotationState{
		Accounts: state.Accounts,
		Groups:   state.Groups,
	})
	state.Config = cfg
	state.Accounts = annotations.Accounts
	state.Groups = annotations.Groups
	return state
}

func diskStateFromStore(store *PluginState) PluginDiskState {
	if store == nil {
		return PluginDiskState{Config: DefaultConfig()}
	}
	annotations := store.Annotations()
	return PluginDiskState{
		Config:   store.Config(),
		Accounts: annotations.Accounts,
		Groups:   annotations.Groups,
	}
}
