package repository

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Pad absent identity fields with JSON null before comparing the current row.
// Keep the predicate on the UPDATE tuple itself: a correlated subquery can
// retain the pre-lock snapshot during PostgreSQL EvalPlanQual rechecks.
func marshalOAuthCredentialIdentity(credentials map[string]any) (identity, nulls string, err error) {
	encoded, err := json.Marshal(service.OAuthCredentialIdentity(credentials))
	if err != nil {
		return "", "", err
	}
	defaults, err := json.Marshal(service.OAuthCredentialIdentity(nil))
	if err != nil {
		return "", "", err
	}
	return string(encoded), string(defaults), nil
}

// Only provider changes are merged. A concurrent model/config edit must neither
// be rolled back nor cause a successfully rotated refresh token to be discarded.
func (r *accountRepository) UpdateOAuthCredentialsIfUnchanged(ctx context.Context, expected *service.Account, credentials map[string]any) (bool, error) {
	patch := map[string]any{}
	for key, value := range credentials {
		if !reflect.DeepEqual(expected.Credentials[key], value) {
			patch[key] = value
		}
	}
	identity, nulls, err := marshalOAuthCredentialIdentity(expected.Credentials)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	key := service.OpenAIUpstreamHealthExtraKey
	if expected.Platform == service.PlatformAnthropic {
		key = service.ClaudeOAuthStateKey
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
	 UPDATE accounts SET credentials=credentials || $2::jsonb,
	 temp_unschedulable_until=CASE WHEN extra->$7->>'kind'='auth_refresh_required'
	   AND temp_unschedulable_reason='OAuth 401: ' || (extra->$7->>'message') THEN NULL ELSE temp_unschedulable_until END,
	 temp_unschedulable_reason=CASE WHEN extra->$7->>'kind'='auth_refresh_required'
	   AND temp_unschedulable_reason='OAuth 401: ' || (extra->$7->>'message') THEN NULL ELSE temp_unschedulable_reason END,
	 extra=COALESCE(extra,'{}'::jsonb)-$7, updated_at=NOW()
	 WHERE id=$1 AND deleted_at IS NULL AND platform=$4 AND type=$5 AND parent_account_id IS NULL
	 AND proxy_id IS NOT DISTINCT FROM $6
	 AND ($8::jsonb || COALESCE(credentials,'{}'::jsonb)) @> $3::jsonb
	 RETURNING id
	) INSERT INTO scheduler_outbox(event_type,account_id,payload) SELECT 'account_changed',id,'{}'::jsonb FROM changed`,
		expected.ID, string(payload), identity, expected.Platform, expected.Type, expected.ProxyID, key, nulls)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, expected.ID)
	}
	return n > 0, err
}

func (r *accountRepository) SaveOAuthAuthenticationState(ctx context.Context, expected *service.Account, state service.OpenAIUpstreamHealth, until time.Time) (bool, error) {
	identity, nulls, err := marshalOAuthCredentialIdentity(expected.Credentials)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	key := service.OpenAIUpstreamHealthExtraKey
	if expected.Platform == service.PlatformAnthropic {
		key = service.ClaudeOAuthStateKey
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
	 UPDATE accounts SET extra=jsonb_set(COALESCE(extra,'{}'::jsonb),ARRAY[$7],$3::jsonb),
	 status=CASE WHEN $8='auth_invalid' THEN 'error' ELSE status END,
	 error_message=CASE WHEN $8='auth_invalid' THEN $9 ELSE error_message END,
	 temp_unschedulable_until=CASE WHEN $8='auth_refresh_required' THEN GREATEST(temp_unschedulable_until,$10) ELSE temp_unschedulable_until END,
	 temp_unschedulable_reason=CASE WHEN $8='auth_refresh_required' AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<=$10) THEN 'OAuth 401: ' || $9 ELSE temp_unschedulable_reason END,
	 updated_at=NOW()
	 WHERE id=$1 AND deleted_at IS NULL AND platform=$4 AND type=$5 AND parent_account_id IS NULL
	 AND proxy_id IS NOT DISTINCT FROM $6
	 AND ($11::jsonb || COALESCE(credentials,'{}'::jsonb)) @> $2::jsonb
	 AND ($8='auth_invalid' OR COALESCE(extra->$7->>'kind','')<>'auth_invalid')
	 RETURNING id
	) INSERT INTO scheduler_outbox(event_type,account_id,payload) SELECT 'account_changed',id,'{}'::jsonb FROM changed`, expected.ID, identity, string(payload), expected.Platform, expected.Type, expected.ProxyID, key, state.Kind, state.Message, until, nulls)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err == nil && n > 0 {
		r.syncSchedulerAccountSnapshotDetached(ctx, expected.ID)
	}
	return n > 0, err
}
