package service

import (
	"testing"
	"time"
)

func TestCodexWindowPresenceUpdatesRequiresThreeObservationsForCleanup(t *testing.T) {
	extra := map[string]any{
		codex5hMissingCountKey: float64(2),
	}
	updates := codexWindowPresenceUpdates(extra, true, false, true)
	if got := updates[codex5hWindowPresentKey]; got != false {
		t.Fatalf("5h presence = %v, want false", got)
	}
	if got := updates[codex5hMissingCountKey]; got != 3 {
		t.Fatalf("5h missing count = %v, want 3", got)
	}
	keys := codexWindowCleanupKeys(updates)
	if len(keys) != 4 {
		t.Fatalf("cleanup keys = %v, want four 5h keys", keys)
	}
	if got := updates[codex7dMissingCountKey]; got != 0 {
		t.Fatalf("7d missing count = %v, want 0", got)
	}
}

func TestCodexWindowPresenceUpdatesRecoveryClearsMissingCount(t *testing.T) {
	updates := codexWindowPresenceUpdates(map[string]any{
		codex5hMissingCountKey: float64(2),
		codex7dMissingCountKey: float64(1),
	}, true, true, false)
	if got := updates[codex5hMissingCountKey]; got != 0 {
		t.Fatalf("recovered 5h missing count = %v, want 0", got)
	}
	if got := updates[codex7dMissingCountKey]; got != 2 {
		t.Fatalf("7d missing count = %v, want 2", got)
	}
	if keys := codexWindowCleanupKeys(updates); len(keys) != 0 {
		t.Fatalf("cleanup before third absence = %v, want none", keys)
	}
}

func TestApplyExtraToUsageDoesNotResurrectKnownAbsentWindow(t *testing.T) {
	usage := &UsageInfo{}
	extra := map[string]any{
		"codex_5h_used_percent": 42.0,
		"codex_5h_window_present": false,
		"codex_7d_used_percent": 17.0,
		"codex_7d_window_present": true,
	}
	applyExtraToUsage(usage, extra, testCodexNow())
	if usage.FiveHour != nil {
		t.Fatal("known absent 5h window was reconstructed")
	}
	if usage.SevenDay == nil {
		t.Fatal("present 7d window was not reconstructed")
	}
}

func testCodexNow() (now time.Time) { return time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC) }
