package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func insertOpenAIWeeklyObservation(ctx context.Context, exec sqlExecutor, accountID int64, generation string, observation *service.OpenAIWeeklyReset) error {
	previous, err := json.Marshal(observation.Previous)
	if err != nil {
		return err
	}
	current, err := json.Marshal(observation.Current)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(struct {
		After *time.Time `json:"observed_after,omitempty"`
		By    *time.Time `json:"observed_by,omitempty"`
	}{observation.ObservedAfter, observation.ObservedBy})
	if err != nil {
		return err
	}
	var start *time.Time
	if !observation.Start.IsZero() {
		start = &observation.Start
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO openai_weekly_quota_observations
		(account_id,generation,period_start,reason,start_source,previous_reading,current_reading,local_requests,local_account_cost,evidence)
		SELECT $1,$2,$3,$4,$5,NULLIF($6::jsonb,'null'::jsonb),NULLIF($7::jsonb,'null'::jsonb),COUNT(*),`+accountWindowCostSQL+`,NULLIF($8::jsonb,'{}'::jsonb)
		FROM usage_logs WHERE account_id=$1 AND created_at >= $3 AND created_at <= $2::text::timestamptz`,
		accountID, generation, start, observation.Reason, observation.Source, string(previous), string(current), string(evidence))
	return err
}

type OpenAIWeeklySeatChange struct {
	AccountID         int64
	Start             time.Time
	ExpectedWeeklyEnd time.Time
	Evidence          string
	Estimated         bool
}

type OpenAIWeeklySeatChangeResult struct {
	AccountID       int64                    `json:"account_id"`
	Name            string                   `json:"account_name"`
	Before          *usagestats.AccountStats `json:"before,omitempty"`
	After           *usagestats.AccountStats `json:"after"`
	UsedPercent     *float64                 `json:"used_percent,omitempty"`
	EstimatedTotal  *float64                 `json:"estimated_total,omitempty"`
	ActivePeriod    bool                     `json:"active_period"`
	AlreadyRecorded bool                     `json:"already_recorded"`
}

// RecordOpenAIWeeklySeatChange previews or records an independently confirmed
// seat change without rewriting the provider's snapshot or existing bills.
func RecordOpenAIWeeklySeatChange(ctx context.Context, db *sql.DB, change OpenAIWeeklySeatChange, apply bool) (*OpenAIWeeklySeatChangeResult, error) {
	if change.AccountID <= 0 || change.Start.IsZero() || !change.Start.Before(change.ExpectedWeeklyEnd) || strings.TrimSpace(change.Evidence) == "" {
		return nil, fmt.Errorf("account ID, seat change, end of the affected weekly window and supporting evidence are required")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !apply, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := &OpenAIWeeklySeatChangeResult{AccountID: change.AccountID}
	var now time.Time
	var start, periodEnd, snapshotEnd sql.NullTime
	var used sql.NullFloat64
	query := `SELECT name, NOW(),
		NULLIF(extra->>'codex_wham_7d_period_start','')::timestamptz,
		NULLIF(extra->>'codex_wham_7d_period_end','')::timestamptz,
		NULLIF(extra->>'codex_wham_7d_reset_at','')::timestamptz,
		NULLIF(extra->>'codex_wham_7d_used_percent','')::float8
		FROM accounts WHERE id=$1 AND platform='openai' AND type='oauth' AND parent_account_id IS NULL AND deleted_at IS NULL`
	if apply {
		query += " FOR UPDATE"
	}
	if err := tx.QueryRowContext(ctx, query, change.AccountID).Scan(&result.Name, &now, &start, &periodEnd, &snapshotEnd, &used); err != nil {
		return nil, fmt.Errorf("load OpenAI OAuth account %d: %w", change.AccountID, err)
	}
	if change.Start.After(now) {
		return nil, fmt.Errorf("seat change is in the future")
	}
	if !periodEnd.Valid {
		periodEnd = snapshotEnd
	}
	result.ActivePeriod = now.Before(change.ExpectedWeeklyEnd) && periodEnd.Valid && periodEnd.Time.Equal(change.ExpectedWeeklyEnd) &&
		snapshotEnd.Valid && snapshotEnd.Time.Equal(change.ExpectedWeeklyEnd)
	if !result.ActivePeriod && now.Before(change.ExpectedWeeklyEnd) {
		return nil, fmt.Errorf("current weekly window does not match the expected end %s", change.ExpectedWeeklyEnd.Format(time.RFC3339))
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM openai_weekly_quota_observations
		WHERE account_id=$1 AND period_start=$2 AND reason='confirmed_seat_change' AND evidence->>'expected_weekly_end'=$3)`,
		change.AccountID, change.Start, change.ExpectedWeeklyEnd.UTC().Format(time.RFC3339Nano)).Scan(&result.AlreadyRecorded); err != nil {
		return nil, err
	}
	end := change.ExpectedWeeklyEnd
	if result.ActivePeriod {
		end = now
		if start.Valid && change.Start.Before(start.Time) && !result.AlreadyRecorded {
			return nil, fmt.Errorf("the recorded current boundary is later than this seat change")
		}
		if start.Valid {
			result.Before, err = queryAccountWindowStats(ctx, tx, change.AccountID, start.Time, &end)
			if err != nil {
				return nil, err
			}
		}
	}
	result.After, err = queryAccountWindowStats(ctx, tx, change.AccountID, change.Start, &end)
	if err != nil {
		return nil, err
	}
	if result.ActivePeriod && used.Valid {
		result.UsedPercent = &used.Float64
		if used.Float64 > 0 && result.After.Cost > 0 {
			estimate := result.After.Cost * 100 / used.Float64
			result.EstimatedTotal = &estimate
		}
	}
	if !apply || result.AlreadyRecorded {
		return result, nil
	}

	metadata, err := json.Marshal(map[string]any{
		"expected_weekly_end": change.ExpectedWeeklyEnd.UTC().Format(time.RFC3339Nano),
		"estimate":            change.Estimated, "detail": change.Evidence, "active_period": result.ActivePeriod,
	})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO openai_weekly_quota_observations
		(account_id,generation,period_start,reason,start_source,local_requests,local_account_cost,evidence)
		VALUES ($1,$2,$3,'confirmed_seat_change','operator_confirmed',$4,$5,$6::jsonb)`, change.AccountID,
		now.UTC().Format("2006-01-02T15:04:05.000000000Z"), change.Start, result.After.Requests, result.After.Cost, string(metadata)); err != nil {
		return nil, err
	}
	if result.ActivePeriod {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET extra = (extra || jsonb_build_object($2::text,$3::text,$4::text,$5::text))
			- 'codex_wham_7d_tracking', updated_at=NOW() WHERE id=$1`, change.AccountID,
			service.OpenAIWeeklyPeriodStartKey, change.Start.UTC().Format(time.RFC3339Nano),
			service.OpenAIWeeklyPeriodEndKey, change.ExpectedWeeklyEnd.UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &change.AccountID, nil, schedulerMetadataOnlyOutboxPayload()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
