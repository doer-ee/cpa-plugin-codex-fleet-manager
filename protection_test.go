package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProtectionThresholdsAndQueue(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	account := weeklyAccount("protected", 0, now.Add(7*24*time.Hour), false)
	account.Annotation.ProtectionEnabled = true
	weeklyUsed, fiveUsed := 89.9, 20.0
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	account.Quota.FiveHour.UsedPercent = &fiveUsed
	cfg := DefaultConfig()
	if protected, _ := accountProtected(account, cfg); protected {
		t.Fatal("11-ish percent remaining should be usable")
	}
	weeklyUsed = 90
	if protected, reason := accountProtected(account, cfg); !protected || reason != "weekly_protected" {
		t.Fatalf("protected=%t reason=%q", protected, reason)
	}
	status, available, reason, _ := accountQueueStateWithConfig(account, cfg, now)
	if status != QueueStatusProtected || available || reason != "weekly_protected" {
		t.Fatalf("status=%s available=%t reason=%q", status, available, reason)
	}
	weeklyUsed = 20
	fiveUsed = 90.01
	if protected, reason := accountProtected(account, cfg); !protected || reason != "five_hour_protected" {
		t.Fatalf("protected=%t reason=%q", protected, reason)
	}
	account.Annotation.ProtectionEnabled = false
	if protected, _ := accountProtected(account, cfg); protected {
		t.Fatal("unprotected account should ignore threshold")
	}
}

func TestProtectionThresholdSettings(t *testing.T) {
	cfg := DefaultConfig()
	if protectionThreshold(cfg.ProtectionFiveHourPercent) != 10 || protectionThreshold(cfg.ProtectionWeeklyPercent) != 10 {
		t.Fatal("default thresholds should be 10%")
	}
	settings := SettingsFromConfig(cfg)
	settings.ProtectionFiveHourPercent = float64Ptr(5)
	settings.ProtectionWeeklyPercent = float64Ptr(15)
	updated, err := ConfigFromSettings(cfg, settings)
	if err != nil || protectionThreshold(updated.ProtectionFiveHourPercent) != 5 || protectionThreshold(updated.ProtectionWeeklyPercent) != 15 {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	settings.ProtectionWeeklyPercent = float64Ptr(101)
	if _, err := ConfigFromSettings(cfg, settings); err == nil {
		t.Fatal("out-of-range threshold accepted")
	}
}

func TestProtectedAccountIsExcludedFromSelection(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	protected := weeklyAccount("protected", 0, now.Add(7*24*time.Hour), false)
	protected.Annotation.ProtectionEnabled = true
	weeklyUsed := 95.0
	protected.Quota.LongWindow.UsedPercent = &weeklyUsed
	available := weeklyAccount("available", 0, now.Add(8*24*time.Hour), false)
	otherUsed := 20.0
	available.Quota.LongWindow.UsedPercent = &otherUsed
	snapshot := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{protected, available}, Now: now}
	decision := PickCodexAccount(requestWithCandidates("protected", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("decision=%#v", decision)
	}
	snapshot.Accounts = []AccountState{protected}
	decision = PickCodexAccount(requestWithCandidates("protected"), snapshot, now)
	if !decision.Handled || decision.AuthID != "" || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("expected CPA Fill First only when no account is usable: %#v", decision)
	}
}

func TestProtectedAccountIsExcludedFromPublishedSelection(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	protected := weeklyAccount("protected", 0, now.Add(7*24*time.Hour), false)
	protected.Annotation.ProtectionEnabled = true
	weeklyUsed := 95.0
	protected.Quota.LongWindow.UsedPercent = &weeklyUsed
	available := weeklyAccount("available", 0, now.Add(8*24*time.Hour), false)
	otherUsed := 20.0
	available.Quota.LongWindow.UsedPercent = &otherUsed
	state := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{protected, available}, Now: now,
		CPAAdmission: CPAAdmissionState{Observed: true, AuthIDs: map[string]struct{}{"protected": {}, "available": {}}}}
	snapshot := schedulerSnapshotFromState(state, nil)
	result := selectAccountSkipping(*snapshot, []Candidate{{ID: "protected", Provider: "codex"}, {ID: "available", Provider: "codex"}}, now, nil, nil)
	if result.AuthID != "available" {
		t.Fatalf("selection=%#v", result)
	}
}

func TestProtectionNearThresholdFiveMinuteChecks(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	account := weeklyAccount("near", 0, now.Add(7*24*time.Hour), false)
	account.Annotation.ProtectionEnabled = true
	account.LastSuccessAt = now
	weeklyUsed := 20.0
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	fiveHourUsed := 60.0 // Exactly 40% remaining is outside the strict below-40% band.
	account.Quota.FiveHour.UsedPercent = &fiveHourUsed
	cfg := DefaultConfig()
	if due := protectionCheckDueAt(account, cfg); !due.IsZero() {
		t.Fatalf("40%% remaining scheduled a check at %s", due)
	}
	fiveHourUsed = 60.1
	if due := protectionCheckDueAt(account, cfg); !due.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("near-threshold due=%s", due)
	}
	account.Refresh.LastFailureKind = RefreshFailureTransient
	account.Refresh.LastFailureAt = now.Add(5 * time.Minute)
	account.Refresh.NextRetryAt = now.Add(6 * time.Minute)
	if due := protectionCheckDueAt(account, cfg); !due.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("failed-check retry due=%s", due)
	}
	fiveHourUsed = 90.0
	if due := protectionCheckDueAt(account, cfg); !due.IsZero() {
		t.Fatalf("protected account scheduled extra check at %s", due)
	}
}

func TestNearThresholdCheckRemainsDueWhileDormant(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	account := weeklyAccount("near", 0, now.Add(7*24*time.Hour), false)
	account.Annotation.ProtectionEnabled = true
	account.LastSuccessAt = now
	weeklyUsed := 20.0
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	fiveHourUsed := 75.0
	account.Quota.FiveHour.UsedPercent = &fiveHourUsed
	store := NewPluginState(DefaultConfig())
	account.Annotation.ProtectionEnabled = false
	store.UpsertQuota(account)
	store.SetAnnotations(AnnotationState{Accounts: map[string]AccountAnnotation{"auth:near": {ProtectionEnabled: true}}})
	if store.RefreshActive(now) {
		t.Fatal("test requires dormant refresh")
	}
	if due := store.NextRefreshDueAt(now); !due.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("dormant deadline=%s", due)
	}
	due := store.DueAccounts(now.Add(5 * time.Minute))
	if len(due) != 1 || due[0].Refresh.DueReason != "protection_check_due" {
		t.Fatalf("dormant due accounts=%#v", due)
	}
}

func TestWeeklyNearThresholdDoesNotTriggerFiveMinuteCheck(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	account := weeklyAccount("weekly-near", 0, now.Add(7*24*time.Hour), false)
	account.Annotation.ProtectionEnabled = true
	account.LastSuccessAt = now
	weeklyUsed := 85.0 // 15% weekly remaining: above protection, below its former early-check band.
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	if due := protectionCheckDueAt(account, DefaultConfig()); !due.IsZero() {
		t.Fatalf("weekly quota alone scheduled an extra check at %s", due)
	}
	weeklyUsed = 90.0
	if protected, reason := accountProtected(account, DefaultConfig()); !protected || reason != "weekly_protected" {
		t.Fatalf("weekly threshold: protected=%t reason=%q", protected, reason)
	}
}
