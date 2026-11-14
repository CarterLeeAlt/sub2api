package repository

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) RecoverClaudeReset(ctx context.Context, a *service.Account, recovery service.ClaudeResetRecovery) (bool, error) {
	identity, nulls, err := marshalOAuthCredentialIdentity(a.Credentials)
	if err != nil {
		return false, err
	}
	models, err := json.Marshal(a.Extra["model_rate_limits"])
	if err != nil {
		return false, err
	}
	updates := map[string]any{}
	if slices.Contains(recovery.Cleared, "five_hour") {
		updates["session_window_utilization"] = 0
	}
	if slices.Contains(recovery.Cleared, "seven_day") {
		updates["passive_usage_7d_utilization"] = 0
	}
	if slices.Contains(recovery.Cleared, "seven_day_overage_included") {
		updates["passive_usage_7d_oi_utilization"] = 0
	}
	patch, err := json.Marshal(updates)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
	 UPDATE accounts SET
	 rate_limited_at=CASE WHEN $3 THEN NULL ELSE rate_limited_at END,
	 rate_limit_reset_at=CASE WHEN $3 THEN NULL ELSE rate_limit_reset_at END,
	 temp_unschedulable_until=CASE WHEN $4 THEN NULL ELSE temp_unschedulable_until END,
	 temp_unschedulable_reason=CASE WHEN $4 THEN NULL ELSE temp_unschedulable_reason END,
	 extra=(CASE WHEN $5 THEN COALESCE(extra,'{}'::jsonb)#-'{model_rate_limits,claude-fable-5}' ELSE COALESCE(extra,'{}'::jsonb) END) || $6::jsonb,
	 updated_at=NOW()
	 WHERE id=$1 AND deleted_at IS NULL AND platform='anthropic' AND type='oauth'
	 AND proxy_id IS NOT DISTINCT FROM $7
	 AND ($13::jsonb || COALESCE(credentials,'{}'::jsonb)) @> $2::jsonb
	 AND rate_limited_at IS NOT DISTINCT FROM $8 AND rate_limit_reset_at IS NOT DISTINCT FROM $9
	 AND temp_unschedulable_until IS NOT DISTINCT FROM $10 AND COALESCE(temp_unschedulable_reason,'')=$11
	 AND COALESCE(extra->'model_rate_limits','null'::jsonb)=$12::jsonb
	 RETURNING id
	) INSERT INTO scheduler_outbox(event_type,account_id,payload) SELECT 'account_changed',id,'{}'::jsonb FROM changed`, a.ID, identity, recovery.ClearGlobal, recovery.ClearThreshold, recovery.ClearFable, string(patch), a.ProxyID, a.RateLimitedAt, a.RateLimitResetAt, a.TempUnschedulableUntil, a.TempUnschedulableReason, string(models), nulls)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, a.ID)
	}
	return n > 0, err
}
