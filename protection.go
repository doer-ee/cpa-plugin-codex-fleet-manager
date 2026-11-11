package main

import (
	"math"
	"time"
)

const protectionCheckInterval = 5 * time.Minute

func float64Ptr(value float64) *float64 { return &value }

func protectionThreshold(value *float64) float64 {
	if value == nil {
		return 10
	}
	return *value
}

func validProtectionThreshold(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

// accountProtected is based on unrounded usage. Missing optional five-hour
// windows do not block an account; an unknown mandatory weekly usage does.
func accountProtected(account AccountState, cfg Config) (bool, string) {
	if !account.Annotation.ProtectionEnabled {
		return false, ""
	}
	if account.Family == AccountFamilyWeekly {
		window := account.Quota.LongWindow
		if window == nil || window.UsedPercent == nil || 100-*window.UsedPercent <= protectionThreshold(cfg.ProtectionWeeklyPercent) {
			return true, "weekly_protected"
		}
	}
	if window := account.Quota.FiveHour; window != nil {
		if window.UsedPercent == nil || 100-*window.UsedPercent <= protectionThreshold(cfg.ProtectionFiveHourPercent) {
			return true, "five_hour_protected"
		}
	}
	return false, ""
}

// A near-threshold account gets a dedicated five-minute quota check. The
// existing Protected state takes over at the configured threshold.
func protectionCheckDueAt(account AccountState, cfg Config) time.Time {
	if !account.Annotation.ProtectionEnabled || account.LastSuccessAt.IsZero() || account.Refresh.AuthFailure || account.Refresh.LastFailureKind == RefreshFailureLocal {
		return time.Time{}
	}
	if protected, _ := accountProtected(account, cfg); protected {
		return time.Time{}
	}
	near := func(window *QuotaWindow, threshold float64) bool {
		if window == nil || window.UsedPercent == nil {
			return false
		}
		remaining := 100 - *window.UsedPercent
		return remaining > threshold && remaining < threshold+20
	}
	if near(account.Quota.FiveHour, protectionThreshold(cfg.ProtectionFiveHourPercent)) ||
		(account.Family == AccountFamilyWeekly && near(account.Quota.LongWindow, protectionThreshold(cfg.ProtectionWeeklyPercent))) {
		due := account.LastSuccessAt.Add(protectionCheckInterval)
		if retryDue := account.Refresh.LastFailureAt.Add(protectionCheckInterval); account.Refresh.LastFailureKind == RefreshFailureTransient && retryDue.After(due) {
			due = retryDue
		}
		if account.Refresh.NextRetryAt.After(due) {
			due = account.Refresh.NextRetryAt
		}
		return due
	}
	return time.Time{}
}
