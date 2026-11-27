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
		return buildCodexWhamWindowExtraUpdates(&OpenAIQuotaUsage{RateLimit: limits, PlanType: plan}, at, false)
	}
	old := report(base.Add(time.Hour), base.Add(week), 70, false, "team")
	e := ApplyOpenAIWeeklyReset(nil, old)
	require.Equal(t, "first_observation", e.Reason)
	require.True(t, base.Equal(e.Start), "use upstream absolute timestamp, not time of HTTP response plus countdown")
	for _, tc := range []struct {
		name, plan, reason, source string
		has5h                      bool
		used                       float64
		end, start                 time.Time
	}{
		{"normal request", "team", "reading_changed", "existing_boundary", false, 71, base.Add(week), base},
		{"seat changed and week restarted between polls", "team", "business_window_layout_changed", "upstream_schedule", true, 16, base.Add(week + 90*time.Minute), base.Add(90 * time.Minute)},
		{"seat restart same scheduled end", "team", "business_window_layout_changed", "first_observation_after_reset", true, 16, base.Add(week), base.Add(2 * time.Hour)},
		{"reset without seat change", "team", "weekly_usage_decreased", "first_observation_after_reset", false, 1, base.Add(week), base.Add(2 * time.Hour)},
		{"plan change even if usage rises", "self_serve_business_prolite", "plan_changed", "first_observation_after_reset", false, 72, base.Add(week), base.Add(2 * time.Hour)},
		{"new schedule after missed zero", "team", "new_window_schedule", "upstream_schedule", false, 72, base.Add(week + 90*time.Minute), base.Add(90 * time.Minute)},
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
	rolled := report(base.Add(week+3*time.Hour), base.Add(2*week), 16, false, "team")
	redeemedAt := base.Add(90 * time.Minute)
	provisional := report(base.Add(2*time.Hour), base.Add(week), 4, false, "team")
	require.Equal(t, "first_observation_after_reset", ApplyOpenAIWeeklyReset(old, provisional).Source)
	for _, source := range []string{"upstream_schedule", "credit_redeemed_at"} {
		end := base.Add(week)
		if source == "upstream_schedule" {
			end = redeemedAt.Add(week)
		}
		next := report(base.Add(3*time.Hour), end, 4, false, "team")
		if source == "credit_redeemed_at" {
			next["codex_wham_credit_redeemed_at"] = redeemedAt.Format(time.RFC3339)
		}
		event := ApplyOpenAIWeeklyReset(provisional, next)
		require.Equal(t, "reset_time_confirmed", event.Reason)
		require.Equal(t, source, event.Source)
		require.True(t, redeemedAt.Equal(event.Start))
	}
	for _, used := range []float64{0, 70} {
		updates := report(base.Add(2*time.Hour), base.Add(week), used, false, "team")
		updates["codex_wham_credit_redeemed_at"] = redeemedAt.Format(time.RFC3339)
		event := ApplyOpenAIWeeklyReset(old, updates)
		if used == 0 {
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
	require.Equal(t, "weekly_window_returned", ApplyOpenAIWeeklyReset(absent, restored).Reason)
	require.Equal(t, base.Add(4*time.Hour).Format(time.RFC3339), restored[OpenAIWeeklyPeriodStartKey])
	require.True(t, base.Add(week).Equal(codexWeeklyStatsStart(old, &UsageProgress{ResetsAt: &e.Current.ResetAt}, base.Add(week+time.Minute))))
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
	require.True(t, change.Start.Equal(first.Start))
}
