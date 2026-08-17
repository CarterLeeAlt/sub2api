package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerOutboxEventForExtraUpdates_QuotaDecisionFieldsUseMetadataEvent(t *testing.T) {
	t.Parallel()

	tests := []map[string]any{
		{
			"codex_5h_used_percent":  98.0,
			"codex_5h_reset_at":      "2026-08-17T12:00:00Z",
			"codex_usage_updated_at": "2026-08-17T07:00:00Z",
		},
		{
			"codex_wham_5h_window_present": true,
			"codex_wham_usage_updated_at":  "2026-08-17T07:00:00.123456789Z",
			"codex_wham_presence_schema":   "wham-usage-v1",
		},
		{
			"session_window_utilization":   0.97,
			"passive_usage_7d_utilization": 0.91,
			"passive_usage_7d_reset":       int64(1786968000),
		},
	}

	for _, updates := range tests {
		require.Equal(t, schedulerExtraUpdateOutboxMetadata, schedulerOutboxKindForExtraUpdates(updates))
		require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(updates))
		for key := range updates {
			require.False(t, isSchedulerNeutralExtraKey(key))
		}
	}
}

func TestSchedulerOutboxEventForExtraUpdates_UIOnlyQuotaFieldsRemainNeutral(t *testing.T) {
	t.Parallel()

	updates := map[string]any{
		"codex_5h_window_minutes":              300,
		"codex_primary_over_secondary_percent": 12.5,
	}
	require.Equal(t, schedulerExtraUpdateOutboxNone, schedulerOutboxKindForExtraUpdates(updates))
	require.False(t, shouldEnqueueSchedulerOutboxForExtraUpdates(updates))
}

func TestSchedulerOutboxEventForExtraUpdates_FullAccountChangeWins(t *testing.T) {
	t.Parallel()

	updates := map[string]any{
		"codex_7d_used_percent": 99.0,
		"mixed_scheduling":      true,
	}
	require.Equal(t, schedulerExtraUpdateOutboxAccount, schedulerOutboxKindForExtraUpdates(updates))
}

func TestSchedulerMetadataOutboxPayloadUsesBackwardCompatibleDeduplicatedAccountEvent(t *testing.T) {
	t.Parallel()
	require.True(t, schedulerOutboxEventSupportsDedup(service.SchedulerOutboxEventAccountChanged))
	payload := schedulerMetadataOnlyOutboxPayload()
	require.Equal(t, map[string]any{service.SchedulerOutboxPayloadMetadataOnly: true}, payload)
	payloadJSON, err := json.Marshal(payload)
	require.NoError(t, err)
	accountID := int64(17)
	require.NotEqual(t,
		schedulerOutboxDedupKey(service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil),
		schedulerOutboxDedupKey(service.SchedulerOutboxEventAccountChanged, &accountID, nil, payloadJSON),
		"metadata retry must not deduplicate away a pending full account refresh",
	)
}

func TestAccountRepository_UpdateOpenAICodexWhamSnapshotIfNewer_IsMonotonicAndAtomic(t *testing.T) {
	generation := "2026-08-17T07:00:00.123456789Z"
	tests := []struct {
		name       string
		affected   int64
		wantUpdate bool
	}{
		{name: "newer snapshot commits metadata event", affected: 1, wantUpdate: true},
		{name: "older out of order snapshot is discarded", affected: 0, wantUpdate: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectBegin()
			mock.ExpectExec(`(?s)`+regexp.QuoteMeta("UPDATE accounts")+`.*`+regexp.QuoteMeta("extra->>'codex_wham_usage_updated_at' <= $3")).
				WithArgs(sqlmock.AnyArg(), int64(17), generation).
				WillReturnResult(sqlmock.NewResult(0, tt.affected))
			if tt.wantUpdate {
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
					WithArgs(service.SchedulerOutboxEventAccountChanged, int64(17), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}

			repo := newAccountRepositoryWithSQL(client, db, nil)
			updated, err := repo.UpdateOpenAICodexWhamSnapshotIfNewer(context.Background(), 17, generation, map[string]any{
				"codex_wham_usage_updated_at":  generation,
				"codex_wham_presence_schema":   "wham-usage-v1",
				"codex_wham_5h_window_present": true,
				"codex_5h_used_percent":        95.0,
			})

			require.NoError(t, err)
			require.Equal(t, tt.wantUpdate, updated)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountRepository_ListAccountsWithSchedulingThresholdPause_FiltersAndPagesInDatabase(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("threshold pause page").
		WithArgs(service.StatusActive, int64(41), `{"source":"account\_scheduling\_threshold"%`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	accounts, err := repo.ListAccountsWithSchedulingThresholdPause(context.Background(), 41, 50)

	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())
	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, `"status" = $1`)
	require.Contains(t, normalized, `"id" > $2`)
	require.Contains(t, normalized, `"temp_unschedulable_reason" LIKE $3`)
	require.Contains(t, normalized, `ORDER BY "accounts"."id" ASC`)
	require.Contains(t, strings.ToUpper(normalized), "LIMIT 50")
}
