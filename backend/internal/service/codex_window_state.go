package service

import "context"

const codexWindowMissingConfirmationThreshold = 3

const (
	codex5hWindowPresentKey = "codex_5h_window_present"
	codex7dWindowPresentKey = "codex_7d_window_present"
	codex5hMissingCountKey  = "codex_5h_window_missing_count"
	codex7dMissingCountKey  = "codex_7d_window_missing_count"
)

// CodexUsageExtraRepository is an optional repository capability used by the
// Codex snapshot paths. It lets the service remove stale derived keys atomically
// after three consecutive authoritative observations that a window is absent,
// without widening AccountRepository (which would break lightweight test repos).
type CodexUsageExtraRepository interface {
	UpdateCodexUsageExtra(ctx context.Context, id int64, updates map[string]any, deleteKeys []string) error
}

// codexWindowKnownAbsent is deliberately false when no presence marker exists.
// Older accounts and partial legacy headers therefore keep their historical
// behaviour until an upstream response explicitly describes window presence.
func codexWindowKnownAbsent(extra map[string]any, window string) bool {
	key := codex5hWindowPresentKey
	if window == "7d" {
		key = codex7dWindowPresentKey
	}
	raw, ok := extra[key]
	if !ok {
		return false
	}
	b, ok := raw.(bool)
	return ok && !b
}

func codexWindowPresenceUpdates(extra map[string]any, known bool, fiveHourPresent, sevenDayPresent bool) map[string]any {
	updates := make(map[string]any)
	if !known {
		return updates
	}
	for _, state := range []struct {
		presentKey, countKey string
		present              bool
	}{
		{codex5hWindowPresentKey, codex5hMissingCountKey, fiveHourPresent},
		{codex7dWindowPresentKey, codex7dMissingCountKey, sevenDayPresent},
	} {
		updates[state.presentKey] = state.present
		if state.present {
			updates[state.countKey] = 0
			continue
		}
		count := 0
		if raw, ok := extra[state.countKey]; ok {
			switch v := raw.(type) {
			case int:
				count = v
			case int64:
				count = int(v)
			case float64:
				count = int(v)
			}
		}
		count++
		updates[state.countKey] = count
	}
	return updates
}

func codexWindowCleanupKeys(updates map[string]any) []string {
	keys := make([]string, 0, 8)
	for _, state := range []struct {
		window, presentKey, countKey string
	}{
		{"5h", codex5hWindowPresentKey, codex5hMissingCountKey},
		{"7d", codex7dWindowPresentKey, codex7dMissingCountKey},
	} {
		present, ok := updates[state.presentKey].(bool)
		count, countOK := updates[state.countKey].(int)
		if ok && !present && countOK && count >= codexWindowMissingConfirmationThreshold {
			prefix := "codex_" + state.window + "_"
			for _, suffix := range []string{"used_percent", "reset_after_seconds", "window_minutes", "reset_at"} {
				keys = append(keys, prefix+suffix)
			}
		}
	}
	return keys
}

func mergeCodexPresenceCounts(extra, updates map[string]any) {
	for _, state := range []struct{ presentKey, countKey string }{
		{codex5hWindowPresentKey, codex5hMissingCountKey},
		{codex7dWindowPresentKey, codex7dMissingCountKey},
	} {
		present, ok := updates[state.presentKey].(bool)
		if !ok {
			continue
		}
		if present {
			updates[state.countKey] = 0
			continue
		}
		count := 0
		if raw, ok := extra[state.countKey]; ok {
			switch v := raw.(type) {
			case int:
				count = v
			case int64:
				count = int(v)
			case float64:
				count = int(v)
			}
		}
		updates[state.countKey] = count + 1
	}
}
