package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type healthQuotaRepo struct {
	stubQuotaAccountRepo
	observations []OpenAIUpstreamHealth
	ownerIDs     []int64
}

func (r *healthQuotaRepo) SaveOpenAIUpstreamHealth(_ context.Context, a *Account, state OpenAIUpstreamHealth) (bool, error) {
	r.observations = append(r.observations, state)
	r.ownerIDs = append(r.ownerIDs, a.ID)
	return true, nil
}

func TestOpenAIUpstreamHealthClassification(t *testing.T) {
	for _, tc := range []struct {
		name, source, body, authMode, refresh, want string
		status                                      int
	}{
		{"revoked", "quota", `{"error":{"code":"token_revoked","message":"revoked"}}`, "", "refresh", "auth_invalid", 401},
		{"invalidated screenshot", "quota", `{"error":{"message":"Encountered invalidated oauth token"}}`, "", "refresh", "auth_invalid", 401},
		{"expired refreshable", "quota", `{"error":{"code":"token_expired"}}`, "", "refresh", "auth_refresh_required", 401},
		{"no refresh", "quota", `{"error":{"message":"unauthorized"}}`, "", "", "auth_invalid", 401},
		{"details permission", "reset_credits", `{"error":"unauthorized"}`, "", "", "permission_denied", 401},
		{"details revoked", "reset_credits", `{"error":{"code":"token_invalidated"}}`, "", "refresh", "auth_invalid", 401},
		{"agent task", "quota", `{"error":{"code":"token_invalidated"}}`, OpenAIAuthModeAgentIdentity, "", "request_failed", 401},
		{"permission", "quota", `{"error":{"message":"permission denied"}}`, "", "refresh", "permission_denied", 403},
		{"rate", "quota", `{}`, "", "refresh", "rate_limited", 429},
		{"balance", "quota", `{}`, "", "refresh", "quota_exhausted", 402},
		{"server", "quota", `{}`, "", "refresh", "request_failed", 503},
		{"network", "quota", `{}`, "", "refresh", "network_error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": tc.authMode, "refresh_token": tc.refresh}}
			state := classifyOpenAIUpstreamHealth(a, tc.source, tc.status, []byte(tc.body))
			require.Equal(t, tc.want, state.Kind)
		})
	}
}

func TestOpenAIUpstreamHealthQuota401PersistsOwnerAndNeverReturnsLogin401(t *testing.T) {
	parent := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "secret-access", "refresh_token": "refresh", "chatgpt_account_id": "workspace"}}
	id := parent.ID
	shadow := &Account{ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &id}
	repo := &healthQuotaRepo{stubQuotaAccountRepo: stubQuotaAccountRepo{accounts: map[int64]*Account{100: parent, 200: shadow}}}
	cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(parent): "secret-access"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "token_invalidated", "message": "Encountered invalidated oauth token secret-access"}})
	}))
	defer server.Close()
	svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(server), nil)
	_, err := svc.QueryUsage(context.Background(), shadow.ID)
	require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
	require.Equal(t, "OPENAI_UPSTREAM_AUTH_INVALID", infraerrors.Reason(err))
	require.Len(t, repo.observations, 1)
	require.Equal(t, []int64{parent.ID}, repo.ownerIDs)
	require.NotContains(t, err.Error(), "secret-access")
	require.NotContains(t, repo.observations[0].Message, "secret-access")
}

func TestOpenAIUpstreamHealthForcesRefreshBeforeExpiry(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, Extra: map[string]any{OpenAIUpstreamHealthExtraKey: map[string]any{"kind": "auth_refresh_required"}}}
	require.True(t, (&OpenAITokenRefresher{}).NeedsRefresh(a, time.Minute))
	a.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
	require.False(t, (&OpenAITokenRefresher{}).NeedsRefresh(a, time.Minute))
}
