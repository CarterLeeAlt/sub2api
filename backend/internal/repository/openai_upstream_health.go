package repository

import (
	"context"
	"encoding/json"
	"maps"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Shadows display their credential owner's diagnostic without changing their
// own status, credentials, or manually configured scheduling switch.
func (r *accountRepository) loadOpenAIParentHealth(ctx context.Context, accounts []*dbent.Account) (map[int64]any, error) {
	parentHealth := make(map[int64]any)
	loaded := make(map[int64]bool)
	for _, a := range accounts {
		if a.ParentAccountID == nil {
			parentHealth[a.ID] = a.Extra[service.OpenAIUpstreamHealthExtraKey]
			loaded[a.ID] = true
		}
	}
	var ids []int64
	for _, a := range accounts {
		if a.Platform == service.PlatformOpenAI && a.ParentAccountID != nil && !loaded[*a.ParentAccountID] {
			ids = append(ids, *a.ParentAccountID)
			loaded[*a.ParentAccountID] = true
		}
	}
	if len(ids) > 0 {
		parents, err := r.client.Account.Query().Where(dbaccount.IDIn(ids...), dbaccount.DeletedAtIsNil()).Select(dbaccount.FieldID, dbaccount.FieldExtra).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, parent := range parents {
			parentHealth[parent.ID] = parent.Extra[service.OpenAIUpstreamHealthExtraKey]
		}
	}
	return parentHealth, nil
}

func projectOpenAIParentHealth(account *service.Account, health map[int64]any) {
	if account.Platform != service.PlatformOpenAI || account.ParentAccountID == nil {
		return
	}
	account.Extra = maps.Clone(account.Extra)
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	delete(account.Extra, service.OpenAIUpstreamHealthExtraKey)
	if value := health[*account.ParentAccountID]; value != nil {
		account.Extra[service.OpenAIUpstreamHealthExtraKey] = value
	}
}

func (r *accountRepository) SaveOpenAIUpstreamHealth(ctx context.Context, account *service.Account, state service.OpenAIUpstreamHealth) (bool, error) {
	credentials, err := json.Marshal(account.Credentials)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	var proxy any
	if account.ProxyID != nil {
		proxy = *account.ProxyID
	}
	result, err := r.sql.ExecContext(ctx, `WITH changed AS (
	 UPDATE accounts SET
	 extra=CASE WHEN $4='ok' THEN COALESCE(extra,'{}'::jsonb)-'openai_upstream_health'
	 ELSE jsonb_set(COALESCE(extra,'{}'::jsonb),'{openai_upstream_health}',$3::jsonb) END,
	 status=CASE WHEN $4='auth_invalid' THEN 'error' ELSE status END,
	 error_message=CASE WHEN $4='auth_invalid' THEN $5 ELSE error_message END,
	 temp_unschedulable_until=CASE WHEN $4='auth_refresh_required' THEN GREATEST(temp_unschedulable_until,NOW()+INTERVAL '10 minutes') ELSE temp_unschedulable_until END,
	 temp_unschedulable_reason=CASE WHEN $4='auth_refresh_required' AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<NOW()+INTERVAL '10 minutes') THEN 'OAuth 401: ' || $5 ELSE temp_unschedulable_reason END,
	 updated_at=NOW()
	 WHERE id=$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth' AND parent_account_id IS NULL
	 AND credentials=$2::jsonb AND proxy_id IS NOT DISTINCT FROM $6
	 AND (extra->'openai_upstream_health'->>'observed_at' IS NULL OR (extra->'openai_upstream_health'->>'observed_at')::timestamptz<$7::timestamptz)
	 AND ($4='auth_invalid' OR COALESCE(extra->'openai_upstream_health'->>'kind','')<>'auth_invalid')
	 AND ($4<>'ok' OR (extra->'openai_upstream_health'->>'source'=$8 AND extra->'openai_upstream_health'->>'kind' NOT IN ('auth_invalid','auth_refresh_required')))
	 AND ($4 IN ('auth_invalid','auth_refresh_required') OR COALESCE(extra->'openai_upstream_health'->>'kind','') NOT IN ('auth_invalid','auth_refresh_required'))
	 RETURNING id
	) INSERT INTO scheduler_outbox(event_type,account_id,payload) SELECT 'account_changed',id,'{}'::jsonb FROM changed`, account.ID, string(credentials), string(payload), state.Kind, state.Message, proxy, state.ObservedAt, state.Source)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		r.syncSchedulerAccountSnapshot(ctx, account.ID)
	}
	return n > 0, nil
}
