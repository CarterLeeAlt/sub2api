package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const OpenAIWeeklyPeriodStartKey = "codex_wham_7d_period_start"

const weeklyQuotaTrackingKey = "codex_wham_7d_tracking"
const weeklyQuotaIdentityKey = "codex_wham_member_identity"
const OpenAIWeeklyPeriodEndKey = "codex_wham_7d_period_end"

// WHAM may retain the scheduled end date after an intervening reset. Keep the
// readings bracketing that change so a later audit can distinguish the observed
// interval from an exact boundary supplied by the upstream schedule.
type weeklyQuotaReading struct {
	ObservedAt         time.Time `json:"observed_at"`
	ResetAt            time.Time `json:"next_reset_at"`
	WindowSeconds      int       `json:"window_seconds,omitempty"`
	ReportedResetAt    int       `json:"upstream_reset_at,omitempty"`
	ResetAfterSeconds  int       `json:"reset_after_seconds"`
	UsedPercent        float64   `json:"used_percent"`
	Plan               string    `json:"plan,omitempty"`
	HasFiveHour        bool      `json:"has_five_hour"`
	WorkspaceMatched   *bool     `json:"workspace_matched,omitempty"`
	MemberMatched      *bool     `json:"member_matched,omitempty"`
	CreditRedeemedAt   string    `json:"credit_redeemed_at,omitempty"`
	CreditWindowsReset int       `json:"credit_windows_reset,omitempty"`
	Identity           string    `json:"identity,omitempty"`
}

type weeklyQuotaTracking struct {
	LastValid       *weeklyQuotaReading `json:"last_valid,omitempty"`
	LastTier        *weeklyQuotaReading `json:"last_tier,omitempty"`
	PendingSchedule *weeklyQuotaReading `json:"pending_schedule,omitempty"`
	PendingAfter    *time.Time          `json:"pending_after,omitempty"`
	ResetAfter      *time.Time          `json:"reset_after,omitempty"`
	ResetBy         *time.Time          `json:"reset_by,omitempty"`
}

func quotaIdentityMatches(expected, reported string) *bool {
	if expected == "" || reported == "" {
		return nil
	}
	match := expected == reported
	return &match
}

// A fresh snapshot carries a pointer; JSONB readback yields a bool or nil.
func quotaMatchObservation(value any) *bool {
	switch v := value.(type) {
	case bool:
		return &v
	case *bool:
		return v
	default:
		return nil
	}
}

type OpenAIWeeklyReset struct {
	Start         time.Time
	Reason        string
	Source        string
	Previous      *weeklyQuotaReading
	Current       *weeklyQuotaReading
	ObservedAfter *time.Time `json:"observed_after,omitempty"`
	ObservedBy    *time.Time `json:"observed_by,omitempty"`
}

func weeklyQuotaIdentity(account *Account, workspace string) string {
	member := strings.TrimSpace(account.GetChatGPTUserID())
	if workspace == "" || member == "" {
		return ""
	}
	return HashIdempotencyKey(fmt.Sprintf("%t:%q:%q", account.IsChatGPTAccountFedRAMP(), workspace, member))
}

func readWeeklyQuota(extra map[string]any) *weeklyQuotaReading {
	if extra[codexWham7dWindowPresentKey] != true {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, codexWhamSnapshotGeneration(extra))
	end := parseSchedulingResetAt(extra["codex_wham_7d_reset_at"])
	used, ok := resolveAccountExtraNumber(extra, codexWham7dUsedPercentKey)
	if err != nil || end == nil || !ok || !(used >= 0 && used <= 100) {
		return nil
	}
	plan, _ := extra["codex_wham_plan_type"].(string)
	hasFiveHour, _ := extra[codexWham5hWindowPresentKey].(bool)
	redeemedAt, _ := extra["codex_wham_credit_redeemed_at"].(string)
	identity, _ := extra[weeklyQuotaIdentityKey].(string)
	return &weeklyQuotaReading{
		ObservedAt: at, ResetAt: *end, UsedPercent: used, Plan: plan, HasFiveHour: hasFiveHour,
		WindowSeconds:      parseExtraInt(extra["codex_wham_7d_window_seconds"]),
		ReportedResetAt:    parseExtraInt(extra["codex_wham_7d_reported_reset_at"]),
		ResetAfterSeconds:  parseExtraInt(extra["codex_wham_7d_reset_after_seconds"]),
		WorkspaceMatched:   quotaMatchObservation(extra["codex_wham_workspace_matched"]),
		MemberMatched:      quotaMatchObservation(extra["codex_wham_member_matched"]),
		CreditRedeemedAt:   redeemedAt,
		CreditWindowsReset: parseExtraInt(extra["codex_wham_credit_windows_reset"]),
		Identity:           identity,
	}
}

// effectiveTier is derived from a member's WHAM response. Generic Business
// labels need the documented Standard layout to corroborate the tier.
func (r *weeklyQuotaReading) effectiveTier() string {
	if r.Identity == "" || !quotaIdentityConfirmed(r.WorkspaceMatched) || !quotaIdentityConfirmed(r.MemberMatched) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(r.Plan)) {
	case "team", "business":
		if r.HasFiveHour {
			return "business_standard"
		}
	case "self_serve_business_prolite":
		if !r.HasFiveHour {
			return "business_premium"
		}
	case "plus", "prolite", "pro", "promax":
		return strings.ToLower(strings.TrimSpace(r.Plan))
	}
	return ""
}

func quotaIdentityConfirmed(match *bool) bool {
	return match != nil && *match
}

func (r *weeklyQuotaReading) changedSince(previous *weeklyQuotaReading) bool {
	return r.UsedPercent != previous.UsedPercent || r.WindowSeconds != previous.WindowSeconds ||
		!r.ResetAt.Equal(previous.ResetAt) || r.ReportedResetAt != previous.ReportedResetAt ||
		r.Plan != previous.Plan || r.HasFiveHour != previous.HasFiveHour || r.CreditRedeemedAt != previous.CreditRedeemedAt ||
		r.CreditWindowsReset != previous.CreditWindowsReset || r.Identity != previous.Identity ||
		!reflect.DeepEqual(r.WorkspaceMatched, previous.WorkspaceMatched) || !reflect.DeepEqual(r.MemberMatched, previous.MemberMatched)
}

// ApplyOpenAIWeeklyReset runs with the account row locked. Only corroborated
// changes move the active billing boundary. Every changed reading is auditable.
func ApplyOpenAIWeeklyReset(old, updates map[string]any) *OpenAIWeeklyReset {
	observed, _ := time.Parse(time.RFC3339Nano, codexWhamSnapshotGeneration(updates))
	lastObserved, _ := time.Parse(time.RFC3339Nano, codexWhamSnapshotGeneration(old))
	if !observed.After(lastObserved) {
		updates[OpenAIWeeklyPeriodStartKey] = old[OpenAIWeeklyPeriodStartKey]
		return nil
	}
	var tracking weeklyQuotaTracking
	if raw, ok := old[weeklyQuotaTrackingKey].(string); ok {
		_ = json.Unmarshal([]byte(raw), &tracking)
	}
	current := readWeeklyQuota(updates)
	previous := tracking.LastValid
	if previous == nil {
		previous = readWeeklyQuota(old)
	}
	if tracking.LastTier == nil && previous != nil && previous.effectiveTier() != "" {
		tracking.LastTier = previous
	}
	saved := parseSchedulingResetAt(old[OpenAIWeeklyPeriodStartKey])
	updates[OpenAIWeeklyPeriodStartKey] = old[OpenAIWeeklyPeriodStartKey]
	if current == nil {
		tracking.PendingSchedule, tracking.PendingAfter = nil, nil
		tracking.LastValid = previous
		storeWeeklyQuotaTracking(updates, tracking)
		if previous != nil && old[codexWham7dWindowPresentKey] == true && updates[codexWham7dWindowPresentKey] == false {
			return &OpenAIWeeklyReset{Reason: "weekly_window_absent", Source: "upstream_observation", Previous: previous}
		}
		return nil
	}
	event := &OpenAIWeeklyReset{Previous: previous, Current: current}
	if current.identityMismatch() {
		event.Reason, event.Source = "weekly_identity_mismatched", "upstream_observation"
		return event
	}
	if previous != nil && current.Identity != "" && previous.Identity != "" && current.Identity != previous.Identity {
		tracking = weeklyQuotaTracking{LastValid: current}
		if current.effectiveTier() != "" {
			tracking.LastTier = current
		}
		storeWeeklyQuotaTracking(updates, tracking)
		event.Reason, event.Source = "weekly_identity_changed", "upstream_observation"
		return event
	}

	boundary := selectWeeklyQuotaBoundary(previous, current, saved, &tracking, event)
	tracking.LastValid = current
	if current.effectiveTier() != "" {
		tracking.LastTier = current
	}
	storeWeeklyQuotaTracking(updates, tracking)
	if boundary {
		updates[OpenAIWeeklyPeriodStartKey] = event.Start.UTC().Format(time.RFC3339Nano)
		updates[OpenAIWeeklyPeriodEndKey] = current.ResetAt.UTC().Format(time.RFC3339Nano)
		return event
	}
	if previous == nil || current.changedSince(previous) {
		event.Reason, event.Source = "reading_changed", "existing_boundary"
		if saved != nil {
			event.Start = *saved
		}
		return event
	}
	return nil
}

func (r *weeklyQuotaReading) identityMismatch() bool {
	return (r.WorkspaceMatched != nil && !*r.WorkspaceMatched) || (r.MemberMatched != nil && !*r.MemberMatched)
}

func storeWeeklyQuotaTracking(updates map[string]any, tracking weeklyQuotaTracking) {
	// All values originate from validated readings and are JSON primitives.
	encoded, err := json.Marshal(tracking)
	if err == nil {
		updates[weeklyQuotaTrackingKey] = string(encoded)
	}
}

func selectWeeklyQuotaBoundary(previous, current *weeklyQuotaReading, saved *time.Time, tracking *weeklyQuotaTracking, event *OpenAIWeeklyReset) bool {
	const week = 7 * 24 * time.Hour
	if saved == nil {
		duration := time.Duration(current.WindowSeconds) * time.Second
		if duration <= 0 {
			duration = week
		}
		start := current.ResetAt.Add(-duration)
		if start.After(current.ObservedAt) {
			start = current.ObservedAt
		}
		event.Start, event.Reason, event.Source = start, "first_observation", "initial_schedule_estimate"
		return true
	}
	if previous == nil || previous.identityMismatch() {
		return false
	}

	newSchedule := current.ReportedResetAt > 0 && current.WindowSeconds > 0 &&
		current.ReportedResetAt != previous.ReportedResetAt
	start := current.ResetAt.Add(-time.Duration(current.WindowSeconds) * time.Second)
	inObservedGap := newSchedule && start.After(previous.ObservedAt) && !start.After(current.ObservedAt) && start.After(*saved)

	if newSchedule && tracking.ResetAfter != nil && tracking.ResetBy != nil &&
		start.After(*tracking.ResetAfter) && !start.After(*tracking.ResetBy) {
		event.Start, event.Reason, event.Source = start, "reset_time_confirmed", "upstream_weekly_schedule"
		event.ObservedAfter, event.ObservedBy = tracking.ResetAfter, tracking.ResetBy
		tracking.ResetAfter, tracking.ResetBy = nil, nil
		return true
	}

	if newSchedule && previous.ReportedResetAt > 0 && !start.Before(previous.ResetAt) && !start.After(current.ObservedAt) && start.After(*saved) {
		event.Start, event.Reason, event.Source = start, "window_rollover", "upstream_weekly_schedule"
	} else if lastTier := tracking.LastTier; lastTier != nil && lastTier.Identity == current.Identity &&
		current.effectiveTier() != "" && current.effectiveTier() != lastTier.effectiveTier() {
		event.Previous = lastTier
		event.ObservedAfter, event.ObservedBy = &lastTier.ObservedAt, &current.ObservedAt
		if newSchedule && start.After(lastTier.ObservedAt) && !start.After(current.ObservedAt) && start.After(*saved) {
			event.Start, event.Reason, event.Source = start, "seat_tier_changed", "upstream_weekly_schedule"
		} else {
			event.Start, event.Reason, event.Source = current.ObservedAt, "seat_tier_changed", "first_confirmed_tier_observation"
			tracking.ResetAfter, tracking.ResetBy = event.ObservedAfter, event.ObservedBy
		}
	} else if receipt := confirmedWeeklyQuotaReceipt(previous, current, *saved); receipt != nil {
		event.Start, event.Reason, event.Source = *receipt, "weekly_credit_redeemed", "credit_redeemed_at"
	} else if pending := tracking.PendingSchedule; pending != nil && current.ReportedResetAt == pending.ReportedResetAt &&
		current.WindowSeconds == pending.WindowSeconds && current.Identity == pending.Identity && start.After(*saved) && !start.After(current.ObservedAt) {
		event.Start, event.Reason, event.Source = start, "new_weekly_schedule_confirmed", "upstream_weekly_schedule"
		event.ObservedAfter, event.ObservedBy = tracking.PendingAfter, &pending.ObservedAt
	} else {
		tracking.PendingSchedule, tracking.PendingAfter = nil, nil
		if inObservedGap && previous.ReportedResetAt > 0 {
			tracking.PendingSchedule, tracking.PendingAfter = current, &previous.ObservedAt
		}
		return false
	}
	tracking.PendingSchedule, tracking.PendingAfter = nil, nil
	if event.Source != "first_confirmed_tier_observation" {
		tracking.ResetAfter, tracking.ResetBy = nil, nil
	}
	return true
}

func confirmedWeeklyQuotaReceipt(previous, current *weeklyQuotaReading, start time.Time) *time.Time {
	receipt := parseSchedulingResetAt(current.CreditRedeemedAt)
	if receipt == nil || !receipt.After(start) || receipt.After(current.ObservedAt) ||
		!receipt.Before(previous.ResetAt) || current.Identity == "" || current.Identity != previous.Identity ||
		!quotaIdentityConfirmed(current.WorkspaceMatched) || !quotaIdentityConfirmed(current.MemberMatched) ||
		!quotaIdentityConfirmed(previous.WorkspaceMatched) || !quotaIdentityConfirmed(previous.MemberMatched) {
		return nil
	}
	// A full credit can reset only some of its covered windows. The count
	// proves the weekly refresh when it matches known main-window coverage.
	if previous.HasFiveHour && current.HasFiveHour && current.CreditWindowsReset == 2 {
		return receipt
	}
	if !previous.HasFiveHour && !current.HasFiveHour && current.CreditWindowsReset == 1 {
		switch current.effectiveTier() {
		case "business_premium", "prolite", "pro", "promax":
			if current.effectiveTier() == previous.effectiveTier() {
				return receipt
			}
		}
	}
	return nil
}
