package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) SetModelCatalogWake(wake func()) { r.modelCatalogWake = wake }
func (r *accountRepository) wakeModelCatalogAfterCommit(ctx context.Context) {
	if r.modelCatalogWake != nil {
		service.AfterAccountCommit(ctx, func(context.Context) { r.modelCatalogWake() })
	}
}

// All accounts observed in this round share a delay measured from its completion.
// A configuration edit clears attempted_at, so a late round cannot postpone it.
func (r *accountRepository) FinishModelCatalogRound(ctx context.Context, startedAt, nextSyncAt time.Time) error {
	_, err := r.sql.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{model_catalog,next_sync_at}',to_jsonb($2::text))
		WHERE deleted_at IS NULL AND extra->'model_catalog'->>'source'='upstream'
		AND NULLIF(extra->'model_catalog'->>'attempted_at','')::timestamptz >= $1`, startedAt, nextSyncAt.Format(time.RFC3339Nano))
	return err
}

func (r *accountRepository) ListDueModelCatalogAccounts(ctx context.Context, afterID int64, limit int) ([]service.Account, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT id FROM accounts WHERE deleted_at IS NULL
		AND platform='openai' AND type='oauth' AND parent_account_id IS NULL
		AND extra->'model_catalog'->>'source'='upstream' AND id>$1
		AND (COALESCE(extra->'model_catalog'->>'next_sync_at','')='' OR (extra->'model_catalog'->>'next_sync_at')::timestamptz<=NOW())
		ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	accounts, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]service.Account, 0, len(accounts))
	byID := map[int64]*service.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	for _, id := range ids {
		if a := byID[id]; a != nil {
			result = append(result, *a)
		}
	}
	return result, nil
}

func (r *accountRepository) SaveModelCatalogObservation(ctx context.Context, a *service.Account, revision, expectedCredentials string, state service.AccountModelCatalog) (bool, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	credentials, err := json.Marshal(a.Credentials)
	if err != nil {
		return false, err
	}
	var proxyID any
	if a.ProxyID != nil {
		proxyID = *a.ProxyID
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
		UPDATE accounts SET credentials=$1::jsonb,
		extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{model_catalog}',$2::jsonb),updated_at=NOW()
		WHERE id=$3 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL
		AND extra->'model_catalog'->>'source'='upstream'
		AND extra->'model_catalog'->>'revision'=$4 AND credentials=$5::jsonb
		AND proxy_id IS NOT DISTINCT FROM $6
		AND COALESCE(extra->'model_catalog'->>'attempted_at','')=$7
		AND NOT COALESCE((extra->>'openai_passthrough')::boolean,false)
		AND NOT COALESCE((extra->>'openai_oauth_passthrough')::boolean,false)
		RETURNING id
	) INSERT INTO scheduler_outbox(event_type,account_id,payload)
	SELECT 'account_changed',id,'{}'::jsonb FROM changed`, string(credentials), string(stateJSON), a.ID, revision, expectedCredentials, proxyID, a.ModelCatalog().AttemptedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		r.syncSchedulerAccountSnapshot(ctx, a.ID)
	}
	return n > 0, nil
}
