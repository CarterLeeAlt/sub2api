package service

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	OpenAICodexRateLimitStateExtraKey = "openai_codex_rate_limit_state"
	openAICodexQuota429Source         = "openai_codex_quota_429"
	openAICodexRateLimitStateVersion  = 1
)

// OpenAICodexRateLimitState records the quota window that produced an
// account-level OpenAI 429. It is stored as one bounded object in Account.Extra.
type OpenAICodexRateLimitState struct {
	Version          int     `json:"version"`
	Source           string  `json:"source"`
	Window           string  `json:"window"`
	ObservedAt       string  `json:"observed_at"`
	ResetAt          string  `json:"reset_at"`
	UsedPercent      float64 `json:"used_percent"`
	ThresholdPercent int     `json:"threshold_percent"`
}

func buildOpenAICodexQuota429State(window string, observedAt, resetAt time.Time, usedPercent float64, thresholdPercent int) string {
	state := OpenAICodexRateLimitState{
		Version:          openAICodexRateLimitStateVersion,
		Source:           openAICodexQuota429Source,
		Window:           strings.TrimSpace(window),
		ObservedAt:       formatCodexWhamSnapshotGeneration(observedAt),
		ResetAt:          resetAt.UTC().Format(time.RFC3339Nano),
		UsedPercent:      usedPercent,
		ThresholdPercent: thresholdPercent,
	}
	payload, _ := json.Marshal(state)
	return string(payload)
}

func parseOpenAICodexQuota429State(extra map[string]any) (*OpenAICodexRateLimitState, string, bool) {
	if len(extra) == 0 {
		return nil, "", false
	}
	raw, ok := extra[OpenAICodexRateLimitStateExtraKey]
	if !ok || raw == nil {
		return nil, "", false
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, "", false
	}
	var state OpenAICodexRateLimitState
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, "", false
	}
	if state.Version != openAICodexRateLimitStateVersion || state.Source != openAICodexQuota429Source {
		return nil, "", false
	}
	if state.Window != "5h" && state.Window != "7d" && state.Window != "all" {
		return nil, "", false
	}
	if _, err := time.Parse(time.RFC3339Nano, state.ObservedAt); err != nil {
		return nil, "", false
	}
	if _, err := time.Parse(time.RFC3339Nano, state.ResetAt); err != nil {
		return nil, "", false
	}
	if state.ThresholdPercent < 1 || state.ThresholdPercent > 100 {
		return nil, "", false
	}
	canonical, err := json.Marshal(state)
	if err != nil {
		return nil, "", false
	}
	return &state, string(canonical), true
}
