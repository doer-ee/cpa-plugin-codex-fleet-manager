package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveDefaultStatePathUsesExplicitDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(stateDirectoryEnvironment, dir)
	if got, want := resolveDefaultStatePath(), filepath.Join(dir, "state.json"); got != want {
		t.Fatalf("resolveDefaultStatePath() = %q, want %q", got, want)
	}
}

func TestMigrateStateDirectoryCopiesKnownArtifactsWithoutOverwriting(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, ".user-data.json"), []byte("source-user-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".runtime-state.json"), []byte("source-runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".telegram-notifications.json"), []byte("source-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "unrelated.txt"), []byte("ignore"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, ".runtime-state.json"), []byte("existing-runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateStateDirectory(source, destination); err != nil {
		t.Fatal(err)
	}
	assertFileContents := func(name, want string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(raw); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	assertFileContents(".user-data.json", "source-user-data")
	assertFileContents(".runtime-state.json", "existing-runtime")
	assertFileContents(".telegram-notifications.json", "source-secret")
	if _, err := os.Stat(filepath.Join(destination, "unrelated.txt")); !os.IsNotExist(err) {
		t.Fatalf("unrelated file migrated: %v", err)
	}
	if err := migrateStateDirectory(source, destination); err != nil {
		t.Fatalf("idempotent migration failed: %v", err)
	}
}

func TestLoadPluginDiskStateResetsLegacyNonChatGPTQuotaEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := []byte(`{
  "config": {
    "HandleEnabled": true,
    "QuotaRefreshInterval": 1800000000000,
    "StaleAfter": 18000000000000,
    "MonthlyMode": "expiry_order",
    "Fallback": "fill-first",
    "EnableUsageFeedback": true,
    "MaxRefreshConcurrency": 1,
    "QuotaEndpoint": "https://example.test/usage",
    "CircuitFailureThreshold": 3,
    "CircuitOpenDuration": 600000000000,
    "CircuitHalfOpenSuccessThreshold": 1,
    "MaxLogEntries": 2000,
    "LogRetention": 86400000000000
  }
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	state, err := LoadPluginDiskState(path)
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if state.Config.QuotaEndpoint != chatGPTQuotaEndpoint {
		t.Fatalf("QuotaEndpoint = %q, want %q", state.Config.QuotaEndpoint, chatGPTQuotaEndpoint)
	}
	if state.Config.QuotaRefreshInterval != 30*time.Minute {
		t.Fatalf("QuotaRefreshInterval = %s, want 30m", state.Config.QuotaRefreshInterval)
	}
}

func TestPluginDiskStateRoundTripsSchedulerPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := PluginDiskState{
		Config: DefaultConfig(),
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "priority", SchedulerPriority: 7},
		},
	}
	if err := SavePluginDiskState(path, state); err != nil {
		t.Fatalf("SavePluginDiskState returned error: %v", err)
	}

	loaded, err := LoadPluginDiskState(path)
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if got := loaded.Accounts["auth:auth-1"].SchedulerPriority; got != 7 {
		t.Fatalf("loaded scheduler priority = %d, want 7", got)
	}
}

func TestLoadPluginDiskStateDefaultsMissingSchedulerPriorityToZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"accounts":{"auth:auth-1":{"alias":"legacy"}}}`), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	loaded, err := LoadPluginDiskState(path)
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if got := loaded.Accounts["auth:auth-1"].SchedulerPriority; got != 0 {
		t.Fatalf("loaded scheduler priority = %d, want 0", got)
	}
}
