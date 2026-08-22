package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSetOpenAICodexQuotaRateLimitedPersistsProvenanceAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	resetAt := time.Now().UTC().Add(time.Hour)
	stateJSON := `{"version":1,"source":"openai_codex_quota_429","window":"5h"}`
	mock.ExpectExec(`(?s)`+regexp.QuoteMeta("WITH updated AS (")+`.*`+
		regexp.QuoteMeta("extra = jsonb_set")+`.*`+
		regexp.QuoteMeta("AND platform = $6")+`.*`+
		regexp.QuoteMeta("AND type = $7")+`.*`+
		regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(sqlmock.AnyArg(), resetAt, service.OpenAICodexRateLimitStateExtraKey, stateJSON, int64(41),
			service.PlatformOpenAI, service.AccountTypeOAuth, service.SchedulerOutboxEventAccountChanged).
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	require.NoError(t, repo.SetOpenAICodexQuotaRateLimited(context.Background(), 41, resetAt, stateJSON))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGenericRateLimitWritesInvalidateCodexQuotaProvenance(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		resetAt := time.Now().UTC().Add(time.Hour)
		mock.ExpectExec(`(?s)`+regexp.QuoteMeta("WITH updated AS (")+`.*`+
			regexp.QuoteMeta("extra = COALESCE(extra, '{}'::jsonb) - $3")+`.*`+
			regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WithArgs(sqlmock.AnyArg(), resetAt, service.OpenAICodexRateLimitStateExtraKey, int64(44), service.SchedulerOutboxEventAccountChanged).
			WillReturnResult(sqlmock.NewResult(1, 1))

		repo := newAccountRepositoryWithSQL(nil, db, nil)
		require.NoError(t, repo.SetRateLimited(context.Background(), 44, resetAt))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("clear", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		mock.ExpectExec(`(?s)`+regexp.QuoteMeta("WITH updated AS (")+`.*`+
			regexp.QuoteMeta("extra = COALESCE(extra, '{}'::jsonb) - $1")+`.*`+
			regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WithArgs(service.OpenAICodexRateLimitStateExtraKey, int64(45), service.SchedulerOutboxEventAccountChanged).
			WillReturnResult(sqlmock.NewResult(1, 1))

		repo := newAccountRepositoryWithSQL(nil, db, nil)
		require.NoError(t, repo.ClearRateLimit(context.Background(), 45))
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestClearOpenAICodexQuotaRateLimitRequiresExactGenerations(t *testing.T) {
	tests := []struct {
		name        string
		affected    int64
		wantCleared bool
	}{
		{name: "exact 429 and WHAM generations", affected: 1, wantCleared: true},
		{name: "concurrent state change", affected: 0, wantCleared: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			limitedAt := time.Now().UTC().Add(-time.Minute)
			resetAt := time.Now().UTC().Add(time.Hour)
			stateJSON := `{"version":1,"source":"openai_codex_quota_429","window":"5h"}`
			generation := "2026-08-22T09:30:00.123456789Z"
			mock.ExpectExec(`(?s)`+regexp.QuoteMeta("WITH updated AS (")+`.*`+
				regexp.QuoteMeta("AND rate_limited_at = $5")+`.*`+
				regexp.QuoteMeta("AND rate_limit_reset_at = $6")+`.*`+
				regexp.QuoteMeta("AND extra->$1 = $7::jsonb")+`.*`+
				regexp.QuoteMeta("AND extra->>'codex_wham_usage_updated_at' = $8")+`.*`+
				regexp.QuoteMeta("AND (overload_until IS NULL OR overload_until <= NOW())")+`.*`+
				regexp.QuoteMeta("AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until <= NOW())")+`.*`+
				regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
				WithArgs(service.OpenAICodexRateLimitStateExtraKey, int64(42), service.PlatformOpenAI, service.AccountTypeOAuth,
					limitedAt, resetAt, stateJSON, generation, service.SchedulerOutboxEventAccountChanged).
				WillReturnResult(sqlmock.NewResult(0, tt.affected))

			repo := newAccountRepositoryWithSQL(nil, db, nil)
			cleared, err := repo.ClearOpenAICodexQuotaRateLimitIfSnapshotUnchanged(
				context.Background(), 42, limitedAt, resetAt, stateJSON, generation,
			)
			require.NoError(t, err)
			require.Equal(t, tt.wantCleared, cleared)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAICodexQuotaRateLimitRepositoryRejectsInvalidState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newAccountRepositoryWithSQL(nil, db, nil)

	require.Error(t, repo.SetOpenAICodexQuotaRateLimited(context.Background(), 43, time.Now(), `{`))
	cleared, err := repo.ClearOpenAICodexQuotaRateLimitIfSnapshotUnchanged(
		context.Background(), 43, time.Now(), time.Now(), `{`, "generation",
	)
	require.NoError(t, err)
	require.False(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())
}
