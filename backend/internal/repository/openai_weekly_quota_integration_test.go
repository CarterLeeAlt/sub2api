//go:build integration

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRecordOpenAIWeeklySeatChange(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	oldStart := time.Now().UTC().Truncate(time.Second).Add(-3 * 24 * time.Hour)
	start, end := oldStart.Add(2*24*time.Hour), oldStart.Add(7*24*time.Hour)
	account := mustCreateAccount(t, client, &service.Account{Name: "weekly-seat-correction", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{service.OpenAIWeeklyPeriodStartKey: oldStart.Format(time.RFC3339),
			"codex_wham_7d_reset_at": end.Format(time.RFC3339), "codex_wham_7d_used_percent": 16}})
	user := mustCreateUser(t, client, &service.User{Email: "weekly-seat-correction-" + uuid.NewString() + "@test.invalid"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-weekly-seat-correction-" + uuid.NewString(), Name: "weekly-seat-correction"})
	for _, period := range []struct {
		at               time.Time
		requests, tokens int
		cost             float64
	}{
		{start.Add(-time.Second), 3111, 466521672, 741.7333},
		{start, 36, 11443695, 6.9697},
		{end, 1, 123, 1.234},
	} {
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO usage_logs
			(user_id, api_key_id, account_id, request_id, model, input_tokens, total_cost, actual_cost, created_at)
			SELECT $1,$2,$3,$4::text || ':' || n,'gpt-5.6',CASE WHEN n=1 THEN $5::integer ELSE 0 END,
			CASE WHEN n=1 THEN $6::numeric ELSE 0 END,CASE WHEN n=1 THEN $6::numeric ELSE 0 END,$7
			FROM generate_series(1,$8::integer) n`, user.ID, key.ID, account.ID, period.at.Format(time.RFC3339), period.tokens, period.cost, period.at, period.requests)
		require.NoError(t, err)
	}
	change := OpenAIWeeklySeatChange{AccountID: account.ID, Start: start, ExpectedWeeklyEnd: end, Evidence: "confirmed downgrade near midnight", Estimated: true}
	preview, err := RecordOpenAIWeeklySeatChange(ctx, integrationDB, change, false)
	require.NoError(t, err)
	require.True(t, preview.ActivePeriod)
	require.False(t, preview.AlreadyRecorded)
	require.EqualValues(t, 3147, preview.Before.Requests)
	require.EqualValues(t, 477965367, preview.Before.Tokens)
	require.InDelta(t, 748.7030, preview.Before.Cost, 1e-8)
	require.EqualValues(t, 36, preview.After.Requests)
	require.EqualValues(t, 11443695, preview.After.Tokens)
	require.InDelta(t, 6.9697, preview.After.Cost, 1e-8)
	require.InDelta(t, 6.9697, preview.After.UserCost, 1e-8)
	require.InDelta(t, 6.9697, preview.After.StandardCost, 1e-8)
	require.Equal(t, 16.0, *preview.UsedPercent)
	require.InDelta(t, 43.560625, *preview.EstimatedTotal, 1e-8)
	var count int
	query := `SELECT count(*) FROM openai_weekly_quota_observations WHERE account_id=$1 AND reason='confirmed_seat_change'`
	require.NoError(t, integrationDB.QueryRowContext(ctx, query, account.ID).Scan(&count))
	require.Zero(t, count, "preview must not mutate the ledger")

	wrongWeek := change
	wrongWeek.ExpectedWeeklyEnd = end.Add(time.Hour)
	_, err = RecordOpenAIWeeklySeatChange(ctx, integrationDB, wrongWeek, true)
	require.ErrorContains(t, err, "does not match")
	result, err := RecordOpenAIWeeklySeatChange(ctx, integrationDB, change, true)
	require.NoError(t, err)
	require.Equal(t, preview, result)
	result, err = RecordOpenAIWeeklySeatChange(ctx, integrationDB, change, true)
	require.NoError(t, err)
	require.True(t, result.AlreadyRecorded)

	// A fresh repository uses the persisted boundary. A late correction of a
	// different historical period leaves this active boundary untouched.
	historical := change
	historical.Start = start.Add(-14 * 24 * time.Hour)
	historical.ExpectedWeeklyEnd = start.Add(-7 * 24 * time.Hour)
	result, err = RecordOpenAIWeeklySeatChange(ctx, integrationDB, historical, true)
	require.NoError(t, err)
	require.False(t, result.ActivePeriod)
	require.EqualValues(t, 0, result.After.Requests)
	repo := NewAccountRepository(client, integrationDB, nil)
	reloaded, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, start.Format(time.RFC3339), reloaded.Extra[service.OpenAIWeeklyPeriodStartKey])
	require.NoError(t, integrationDB.QueryRowContext(ctx, query, account.ID).Scan(&count))
	require.Equal(t, 2, count)
	var original int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs WHERE account_id=$1`, account.ID).Scan(&original))
	require.Equal(t, 3148, original, "preserve the original ledger")

	writer := repo.(service.OpenAICodexWhamSnapshotRepository)
	writeReading := func(observed time.Time, used int) (bool, error) {
		generation := observed.Format("2006-01-02T15:04:05.000000000Z")
		return writer.UpdateOpenAICodexWhamSnapshotIfNewer(ctx, account.ID, generation, map[string]any{
			"codex_wham_usage_updated_at": generation, "codex_wham_7d_window_present": true, "codex_wham_7d_used_percent": used,
			"codex_wham_7d_reset_at": end.Format(time.RFC3339), "codex_wham_7d_window_seconds": 604800,
		})
	}
	observed := time.Now().UTC()
	applied, err := writeReading(observed, 30)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = writeReading(observed.Add(-time.Minute), 2)
	require.NoError(t, err)
	require.False(t, applied)
	reloaded, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, start.Format(time.RFC3339), reloaded.Extra[service.OpenAIWeeklyPeriodStartKey])
	require.Equal(t, float64(30), reloaded.Extra["codex_wham_7d_used_percent"])
}

func TestWeeklyQuotaBoundaryWithConcurrentRefreshes(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{Name: "weekly-only-quota", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth})
	base := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	change := base.Add(time.Hour)
	const week = 7 * 24 * time.Hour
	write := func(observed, start time.Time, plan string, used int) error {
		repo := NewAccountRepository(client, integrationDB, nil).(service.OpenAICodexWhamSnapshotRepository)
		generation := observed.Format("2006-01-02T15:04:05.000000000Z")
		_, err := repo.UpdateOpenAICodexWhamSnapshotIfNewer(ctx, account.ID, generation, map[string]any{
			"codex_wham_usage_updated_at": generation, "codex_wham_7d_window_present": true, "codex_wham_5h_window_present": false,
			"codex_wham_7d_used_percent": used, "codex_wham_7d_reset_at": start.Add(week).Format(time.RFC3339),
			"codex_wham_7d_reported_reset_at": start.Add(week).Unix(), "codex_wham_7d_window_seconds": int(week.Seconds()),
			"codex_wham_plan_type": plan, "codex_wham_member_identity": "weekly-identity", "codex_wham_member_matched": true,
			"codex_wham_workspace_matched": true,
		})
		return err
	}
	require.NoError(t, write(base.Add(time.Minute), base, "prolite", 30))
	const samples = 8
	results := make(chan error, samples)
	for i := 1; i <= samples; i++ {
		go func() { results <- write(change.Add(time.Duration(i)*time.Minute), change, "pro", 60+i) }()
	}
	for range samples {
		require.NoError(t, <-results)
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM openai_weekly_quota_observations
		WHERE account_id=$1 AND reason='seat_tier_changed' AND period_start=$2`, account.ID, change).Scan(&count))
	require.Equal(t, 1, count)
	reloaded, err := NewAccountRepository(client, integrationDB, nil).GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, change.Format(time.RFC3339), reloaded.Extra[service.OpenAIWeeklyPeriodStartKey])
	require.Equal(t, float64(60+samples), reloaded.Extra["codex_wham_7d_used_percent"])
}
