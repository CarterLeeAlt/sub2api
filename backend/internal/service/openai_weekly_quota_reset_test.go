package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWeeklyQuotaResetEvidence(t *testing.T) {
	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	report := func(at, end time.Time, used float64, has5h bool, plan string) map[string]any {
		weekly := &OpenAIRateLimitWindow{UsedPercent: used, LimitWindowSeconds: int64(week.Seconds()), ResetAt: end.Unix(), ResetAfterSeconds: int64(end.Sub(at).Seconds()) - 2}
		limits := &OpenAIRateLimit{PrimaryWindow: weekly}
		if has5h {
			limits.SecondaryWindow = weekly
			limits.PrimaryWindow = &OpenAIRateLimitWindow{LimitWindowSeconds: 18000, UsedPercent: 100, ResetAfterSeconds: 12}
		}
		matched := true
		return buildCodexWhamWindowExtraUpdates(&OpenAIQuotaUsage{RateLimit: limits, PlanType: plan, weeklyIdentity: "same-member",
			workspaceMatched: &matched, memberMatched: &matched}, at, false)
	}
	old := report(base.Add(time.Hour), base.Add(week), 70, true, "team")
	e := ApplyOpenAIWeeklyReset(nil, old)
	require.Equal(t, "first_observation", e.Reason)
	require.True(t, base.Equal(e.Start), "use upstream absolute timestamp, not time of HTTP response plus countdown")
	for _, tc := range []struct {
		name, plan, reason, source string
		has5h                      bool
		used                       float64
		end, start                 time.Time
	}{
		{"normal request", "team", "reading_changed", "existing_boundary", true, 71, base.Add(week), base},
		{"same standard tier under another label", "business", "reading_changed", "existing_boundary", true, 71, base.Add(week), base},
		{"single absent short window", "team", "reading_changed", "existing_boundary", false, 71, base.Add(week), base},
		{"unrecognized plan label", "future_business_plan", "reading_changed", "existing_boundary", false, 71, base.Add(week), base},
		{"upgrade between polls after heavy use", "self_serve_business_prolite", "seat_tier_changed", "upstream_weekly_schedule", false, 95, base.Add(week + 90*time.Minute), base.Add(90 * time.Minute)},
		{"weekly usage drop", "team", "reading_changed", "existing_boundary", true, 1, base.Add(week), base},
		{"recognized seat with unchanged end", "self_serve_business_prolite", "seat_tier_changed", "first_confirmed_tier_observation", false, 72, base.Add(week), base.Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := base.Add(2 * time.Hour)
			updates := report(now, tc.end, tc.used, tc.has5h, tc.plan)
			event := ApplyOpenAIWeeklyReset(old, updates)
			require.NotNil(t, event)
			require.Equal(t, tc.reason, event.Reason)
			require.Equal(t, tc.source, event.Source)
			require.Equal(t, 70., event.Previous.UsedPercent)
			require.Equal(t, tc.used, event.Current.UsedPercent)
			require.True(t, tc.start.Equal(codexWeeklyStatsStart(updates, &UsageProgress{ResetsAt: &tc.end}, now)))
			unchanged := report(now.Add(time.Minute), tc.end, tc.used, tc.has5h, tc.plan)
			require.Nil(t, ApplyOpenAIWeeklyReset(updates, unchanged))
			require.Equal(t, updates[OpenAIWeeklyPeriodStartKey], unchanged[OpenAIWeeklyPeriodStartKey])
		})
	}
	rolled := report(base.Add(week+3*time.Hour), base.Add(2*week), 16, true, "team")
	redeemedAt := base.Add(90 * time.Minute)
	provisional := report(base.Add(2*time.Hour), base.Add(week), 95, false, "self_serve_business_prolite")
	require.Equal(t, "first_confirmed_tier_observation", ApplyOpenAIWeeklyReset(old, provisional).Source)
	next := report(base.Add(3*time.Hour), redeemedAt.Add(week), 95, false, "self_serve_business_prolite")
	confirmed := ApplyOpenAIWeeklyReset(provisional, next)
	require.Equal(t, "reset_time_confirmed", confirmed.Reason)
	require.Equal(t, "upstream_weekly_schedule", confirmed.Source)
	require.True(t, redeemedAt.Equal(confirmed.Start))

	for _, windows := range []int{1, 2} {
		updates := report(base.Add(2*time.Hour), base.Add(week), 95, true, "team")
		updates["codex_wham_credit_redeemed_at"] = redeemedAt.Format(time.RFC3339)
		updates["codex_wham_credit_windows_reset"] = windows
		event := ApplyOpenAIWeeklyReset(old, updates)
		if windows == 2 {
			require.Equal(t, "credit_redeemed_at", event.Source)
			require.True(t, event.Start.Equal(redeemedAt))
		} else {
			require.True(t, event.Start.Equal(base), "5h-only redemption must retain the weekly boundary")
		}
	}
	require.Equal(t, "window_rollover", ApplyOpenAIWeeklyReset(old, rolled).Reason)
	require.Equal(t, base.Add(week).Format(time.RFC3339), rolled[OpenAIWeeklyPeriodStartKey])
	absent := buildCodexWhamWindowExtraUpdates(&OpenAIQuotaUsage{}, base.Add(3*time.Hour), false)
	require.Equal(t, "weekly_window_absent", ApplyOpenAIWeeklyReset(old, absent).Reason)
	restored := report(base.Add(4*time.Hour), base.Add(week), 2, true, "team")
	require.Equal(t, "reading_changed", ApplyOpenAIWeeklyReset(absent, restored).Reason)
	require.Equal(t, base.Format(time.RFC3339), restored[OpenAIWeeklyPeriodStartKey])
	require.True(t, base.Equal(codexWeeklyStatsStart(old, &UsageProgress{ResetsAt: &e.Current.ResetAt}, base.Add(week+time.Minute))))

	weeklyOnly := report(base.Add(time.Hour), base.Add(week), 30, false, "prolite")
	ApplyOpenAIWeeklyReset(nil, weeklyOnly)
	upgraded := report(base.Add(2*time.Hour), base.Add(week), 60, false, "pro")
	require.Equal(t, "seat_tier_changed", ApplyOpenAIWeeklyReset(weeklyOnly, upgraded).Reason)
	weeklyReset := report(base.Add(2*time.Hour), base.Add(week), 60, false, "prolite")
	weeklyReset["codex_wham_credit_redeemed_at"] = redeemedAt.Format(time.RFC3339)
	weeklyReset["codex_wham_credit_windows_reset"] = 1
	require.Equal(t, "credit_redeemed_at", ApplyOpenAIWeeklyReset(weeklyOnly, weeklyReset).Source)

	newSchedule := report(base.Add(2*time.Hour), redeemedAt.Add(week), 0, false, "prolite")
	require.Equal(t, "existing_boundary", ApplyOpenAIWeeklyReset(weeklyOnly, newSchedule).Source)
	repeated := report(base.Add(3*time.Hour), redeemedAt.Add(week), 60, false, "prolite")
	require.Equal(t, "new_weekly_schedule_confirmed", ApplyOpenAIWeeklyReset(newSchedule, repeated).Reason)
	require.Equal(t, redeemedAt.Format(time.RFC3339), repeated[OpenAIWeeklyPeriodStartKey])
}

func TestWeeklyQuotaEvidenceRecordsIdentityWithoutInferringMissingIDs(t *testing.T) {
	observed := time.Date(2026, 11, 28, 1, 28, 3, 0, time.UTC)
	end := observed.Add(3 * 24 * time.Hour)
	usage := &OpenAIQuotaUsage{PlanType: "team", RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 604800, ResetAt: end.Unix(), UsedPercent: 4}}}
	usage.workspaceMatched = quotaIdentityMatches("workspace", "workspace")
	usage.memberMatched = quotaIdentityMatches("member-21", "")
	previous := buildCodexWhamWindowExtraUpdates(usage, observed, false)
	first := ApplyOpenAIWeeklyReset(nil, previous)
	require.True(t, *first.Current.WorkspaceMatched)
	require.Nil(t, first.Current.MemberMatched)
	usage.memberMatched = quotaIdentityMatches("member-21", "member-50")
	next := buildCodexWhamWindowExtraUpdates(usage, observed.Add(time.Minute), false)
	change := ApplyOpenAIWeeklyReset(previous, next)
	require.NotNil(t, change)
	require.False(t, *change.Current.MemberMatched)
	require.Equal(t, "weekly_identity_mismatched", change.Reason)
	require.Equal(t, previous[OpenAIWeeklyPeriodStartKey], next[OpenAIWeeklyPeriodStartKey])
}
