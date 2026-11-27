package service

import (
	"reflect"
	"strings"
	"time"
)

const OpenAIWeeklyPeriodStartKey = "codex_wham_7d_period_start"

const weeklyResetObservedAfterKey = "codex_wham_7d_reset_observed_after"
const weeklyResetReasonKey = "codex_wham_7d_period_reason"

// WHAM may retain the scheduled end date after an intervening reset. Keep the
// readings bracketing that change so a later audit can distinguish the observed
// interval from an exact boundary supplied by the upstream schedule.
type weeklyQuotaReading struct {
	ObservedAt        time.Time `json:"observed_at"`
	ResetAt           time.Time `json:"next_reset_at"`
	WindowSeconds     int       `json:"window_seconds,omitempty"`
	ReportedResetAt   int       `json:"upstream_reset_at,omitempty"`
	ResetAfterSeconds int       `json:"reset_after_seconds"`
	UsedPercent       float64   `json:"used_percent"`
	Plan              string    `json:"plan,omitempty"`
	HasFiveHour       bool      `json:"has_five_hour"`
	WorkspaceMatched  *bool     `json:"workspace_matched,omitempty"`
	MemberMatched     *bool     `json:"member_matched,omitempty"`
	CreditRedeemedAt  string    `json:"credit_redeemed_at,omitempty"`
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
	Start    time.Time
	Reason   string
	Source   string
	Previous *weeklyQuotaReading
	Current  *weeklyQuotaReading
}

func readWeeklyQuota(extra map[string]any) *weeklyQuotaReading {
	if extra[codexWham7dWindowPresentKey] != true {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, codexWhamSnapshotGeneration(extra))
	end := parseSchedulingResetAt(extra["codex_wham_7d_reset_at"])
	used, ok := resolveAccountExtraNumber(extra, codexWham7dUsedPercentKey)
	if err != nil || end == nil || !ok || used < 0 || used > 100 {
		return nil
	}
	plan, _ := extra["codex_wham_plan_type"].(string)
	hasFiveHour, _ := extra[codexWham5hWindowPresentKey].(bool)
	redeemedAt, _ := extra["codex_wham_credit_redeemed_at"].(string)
	return &weeklyQuotaReading{
		ObservedAt: at, ResetAt: *end, UsedPercent: used, Plan: plan, HasFiveHour: hasFiveHour,
		WindowSeconds:     parseExtraInt(extra["codex_wham_7d_window_seconds"]),
		ReportedResetAt:   parseExtraInt(extra["codex_wham_7d_reported_reset_at"]),
		ResetAfterSeconds: parseExtraInt(extra["codex_wham_7d_reset_after_seconds"]),
		WorkspaceMatched:  quotaMatchObservation(extra["codex_wham_workspace_matched"]),
		MemberMatched:     quotaMatchObservation(extra["codex_wham_member_matched"]),
		CreditRedeemedAt:  redeemedAt,
	}
}

func businessQuotaPlan(plan string) bool {
	return plan == "team" || plan == "business" || strings.HasPrefix(plan, "self_serve_business_")
}

func (r *weeklyQuotaReading) changedSince(previous *weeklyQuotaReading) bool {
	return r.UsedPercent != previous.UsedPercent || r.WindowSeconds != previous.WindowSeconds ||
		!r.ResetAt.Equal(previous.ResetAt) || r.ReportedResetAt != previous.ReportedResetAt ||
		r.Plan != previous.Plan || r.HasFiveHour != previous.HasFiveHour || r.CreditRedeemedAt != previous.CreditRedeemedAt ||
		!reflect.DeepEqual(r.WorkspaceMatched, previous.WorkspaceMatched) || !reflect.DeepEqual(r.MemberMatched, previous.MemberMatched)
}

// ApplyOpenAIWeeklyReset runs with the account row locked. It carries the same
// start across normal samples, including repeated zeroes and 5h-only rollovers.
func ApplyOpenAIWeeklyReset(old, updates map[string]any) *OpenAIWeeklyReset {
	current := readWeeklyQuota(updates)
	previous := readWeeklyQuota(old)
	if current == nil {
		updates[OpenAIWeeklyPeriodStartKey] = nil
		updates[weeklyResetObservedAfterKey] = nil
		updates[weeklyResetReasonKey] = nil
		if previous != nil && updates[codexWham7dWindowPresentKey] == false {
			return &OpenAIWeeklyReset{Reason: "weekly_window_absent", Source: "upstream_observation", Previous: previous}
		}
		return nil
	}
	if previous != nil && !current.ObservedAt.After(previous.ObservedAt) {
		updates[OpenAIWeeklyPeriodStartKey] = old[OpenAIWeeklyPeriodStartKey]
		return nil
	}
	var reason string
	switch {
	case previous == nil && old[codexWham7dWindowPresentKey] == false:
		reason = "weekly_window_returned"
	case previous == nil:
		reason = "first_observation"
	case !current.ObservedAt.Before(previous.ResetAt) && current.ResetAt.After(previous.ResetAt):
		reason = "window_rollover"
	case previous.Plan != "" && current.Plan != "" && current.Plan != previous.Plan:
		reason = "plan_changed"
	case businessQuotaPlan(current.Plan) && previous.HasFiveHour != current.HasFiveHour:
		reason = "business_window_layout_changed"
	case current.UsedPercent < previous.UsedPercent:
		reason = "weekly_usage_decreased"
	}
	seconds := current.WindowSeconds
	if seconds <= 0 {
		seconds = 7 * 24 * 60 * 60
	}
	start := current.ResetAt.Add(-time.Duration(seconds) * time.Second)
	if reason == "" {
		saved := parseSchedulingResetAt(old[OpenAIWeeklyPeriodStartKey])
		// A refresh can see the weekly drop while credit consumption is still
		// being confirmed, or before WHAM publishes the new end date. Refine
		// that provisional boundary only with evidence inside the original gap.
		if lower := parseSchedulingResetAt(old[weeklyResetObservedAfterKey]); lower != nil && saved != nil {
			var confirmed *time.Time
			source := "upstream_schedule"
			if current.ReportedResetAt > 0 && !current.ResetAt.Equal(previous.ResetAt) && start.After(*lower) && !start.After(*saved) {
				confirmed = &start
			}
			if receipt := parseSchedulingResetAt(current.CreditRedeemedAt); old[weeklyResetReasonKey] == "weekly_usage_decreased" && receipt != nil && receipt.After(*lower) && !receipt.After(*saved) {
				confirmed, source = receipt, "credit_redeemed_at"
			}
			if confirmed != nil {
				updates[OpenAIWeeklyPeriodStartKey] = confirmed.UTC().Format(time.RFC3339Nano)
				updates[weeklyResetObservedAfterKey] = nil
				return &OpenAIWeeklyReset{Start: *confirmed, Reason: "reset_time_confirmed", Source: source, Previous: previous, Current: current}
			}
		}
		switch {
		case saved == nil:
			reason = "first_observation"
		case current.ResetAt.After(previous.ResetAt) && start.After(previous.ObservedAt) && !start.After(current.ObservedAt) && current.UsedPercent > 0:
			reason = "new_window_schedule"
		default:
			updates[OpenAIWeeklyPeriodStartKey] = old[OpenAIWeeklyPeriodStartKey]
			if current.changedSince(previous) {
				return &OpenAIWeeklyReset{Start: *saved, Reason: "reading_changed", Source: "existing_boundary", Previous: previous, Current: current}
			}
			return nil
		}
	}
	source := "upstream_schedule"
	if current.ReportedResetAt == 0 {
		source = "relative_countdown"
	}
	lastAt, _ := time.Parse(time.RFC3339Nano, codexWhamSnapshotGeneration(old))
	if start.After(current.ObservedAt) || (reason != "first_observation" && start.Before(lastAt)) {
		// The reset is only known to have happened by this reading. Do not
		// attach the old seat's charges or label this timestamp as exact.
		start, source = current.ObservedAt, "first_observation_after_reset"
	}
	// A receipt counts affected windows without naming them. A drop in the
	// weekly reading before the scheduled rollover corroborates a weekly reset.
	if reason == "weekly_usage_decreased" && current.ObservedAt.Before(previous.ResetAt) {
		if receipt := parseSchedulingResetAt(current.CreditRedeemedAt); receipt != nil && receipt.After(lastAt) && !receipt.After(current.ObservedAt) {
			start, source = *receipt, "credit_redeemed_at"
		}
	}
	updates[OpenAIWeeklyPeriodStartKey] = start.UTC().Format(time.RFC3339Nano)
	updates[weeklyResetReasonKey] = reason
	updates[weeklyResetObservedAfterKey] = nil
	if source == "first_observation_after_reset" && !lastAt.IsZero() {
		updates[weeklyResetObservedAfterKey] = lastAt.UTC().Format(time.RFC3339Nano)
	}
	return &OpenAIWeeklyReset{Start: start, Reason: reason, Source: source, Previous: previous, Current: current}
}
