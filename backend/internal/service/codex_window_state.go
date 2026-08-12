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

// codexWindowKnownAbsent remains false until three consecutive authoritative
// misses. Older accounts, partial headers, and transient omissions therefore
// keep their historical window until upstream absence is confirmed.
func codexWindowKnownAbsent(extra map[string]any, window string) bool {
	present, ok := codexWindowPresence(extra, window)
	return ok && !present && codexWindowMissingCount(extra, window) >= codexWindowMissingConfirmationThreshold
}

func codexWindowPresenceKnown(extra map[string]any, window string) bool {
	present, ok := codexWindowPresence(extra, window)
	if !ok {
		return false
	}
	// A present window is authoritative immediately. Absence deliberately
	// remains pending until three consecutive observations, so refresh callers
	// keep probing (subject to their TTL) until the confirmation threshold is
	// reached instead of getting stuck after the first miss.
	return present || codexWindowMissingCount(extra, window) >= codexWindowMissingConfirmationThreshold
}

func codexWindowPresence(extra map[string]any, window string) (bool, bool) {
	key := codex5hWindowPresentKey
	if window == "7d" {
		key = codex7dWindowPresentKey
	}
	present, ok := extra[key].(bool)
	return present, ok
}

func codexWindowMissingCount(extra map[string]any, window string) int {
	key := codex5hMissingCountKey
	if window == "7d" {
		key = codex7dMissingCountKey
	}
	switch value := extra[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func codexWindowHasLegacySnapshot(extra map[string]any, window string) bool {
	prefix := "codex_5h_"
	if window == "7d" {
		prefix = "codex_7d_"
	}
	for _, suffix := range []string{"used_percent", "reset_after_seconds", "window_minutes", "reset_at"} {
		if _, ok := extra[prefix+suffix]; ok {
			return true
		}
	}
	return false
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
		count := codexWindowMissingCount(extra, windowFromCodexPresentKey(state.presentKey))
		count++
		updates[state.countKey] = count
	}
	return updates
}

func windowFromCodexPresentKey(presentKey string) string {
	if presentKey == codex7dWindowPresentKey {
		return "7d"
	}
	return "5h"
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
		count := codexWindowMissingCount(extra, windowFromCodexPresentKey(state.presentKey))
		updates[state.countKey] = count + 1
	}
}
