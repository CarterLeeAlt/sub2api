package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/imroc/req/v3"
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

func TestOpenAIQuotaReadFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		cancel             bool
	}{
		{name: "invalid JSON", body: `{"rate_limit":`, reason: "OPENAI_QUOTA_UPSTREAM_INVALID_RESPONSE"},
		{name: "wrong field type", body: `{"rate_limit":true}`, reason: "OPENAI_QUOTA_UPSTREAM_INVALID_RESPONSE"},
		{name: "caller canceled", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "secret", "chatgpt_account_id": "workspace"}}
			repo := &healthQuotaRepo{stubQuotaAccountRepo: stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "secret"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.cancel {
					cancel()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(server), nil)
			_, err := svc.QueryUsageWindows(ctx, a.ID)
			require.Error(t, err)
			if tc.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.Equal(t, tc.reason, infraerrors.Reason(err))
			}
			for _, observation := range repo.observations {
				require.NotEqual(t, "network_error", observation.Kind)
			}
			if tc.cancel {
				require.Empty(t, repo.observations, "caller cancellation must not alter account health")
			}
		})
	}
}

func TestOpenAIQuotaReadRequestsHaveIndependentDeadlines(t *testing.T) {
	a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "secret", "chatgpt_account_id": "workspace"}}
	repo := &healthQuotaRepo{stubQuotaAccountRepo: stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}}
	cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "secret"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/backend-api/wham/usage" {
			timer := time.NewTimer(50 * time.Millisecond)
			defer timer.Stop()
			<-timer.C
			_, _ = w.Write([]byte(`{"rate_limit":null}`))
		} else {
			_, _ = w.Write([]byte(`{"available_count":2}`))
		}
	}))
	defer server.Close()
	var deadlines []time.Time
	factory := func(proxy string) (*req.Client, error) {
		client, err := newQuotaRedirectingFactory(server)(proxy)
		client.WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
			return func(r *req.Request) (*req.Response, error) {
				deadline, ok := r.Context().Deadline()
				require.True(t, ok)
				deadlines = append(deadlines, deadline)
				return rt.RoundTrip(r)
			}
		})
		return client, err
	}
	svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), factory, nil)
	usage, err := svc.QueryUsage(context.Background(), a.ID)
	require.NoError(t, err)
	require.Nil(t, usage.Warning)
	require.Len(t, deadlines, 2)
	require.GreaterOrEqual(t, deadlines[1].Sub(deadlines[0]), 50*time.Millisecond, "details must not inherit time already spent fetching usage")
}

func TestOpenAIQuotaReadRetriesBeforePublishingFailure(t *testing.T) {
	for _, tc := range []struct {
		name, source, wantKind  string
		status, failures, calls int
		network                 bool
	}{
		{name: "transient connection", source: "quota", network: true, failures: 1, calls: 2},
		{name: "persistent connection", source: "quota", network: true, failures: 2, calls: 2, wantKind: "network_error"},
		{name: "transient 502", source: "quota", status: 502, failures: 1, calls: 2},
		{name: "transient 503", source: "quota", status: 503, failures: 1, calls: 2},
		{name: "persistent 504", source: "quota", status: 504, failures: 2, calls: 2, wantKind: "request_failed"},
		{name: "authentication", source: "quota", status: 401, failures: 2, calls: 1, wantKind: "auth_invalid"},
		{name: "rate limited", source: "quota", status: 429, failures: 2, calls: 1, wantKind: "rate_limited"},
		{name: "transient details", source: "reset_credits", network: true, failures: 1, calls: 2},
		{name: "persistent details", source: "reset_credits", network: true, failures: 2, calls: 2, wantKind: "network_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "secret", "chatgpt_account_id": "workspace"}}
			repo := &healthQuotaRepo{stubQuotaAccountRepo: stubQuotaAccountRepo{accounts: map[int64]*Account{a.ID: a}}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "secret"}}
			endpoint := "/backend-api/wham/usage"
			if tc.source == "reset_credits" {
				endpoint = "/backend-api/wham/rate-limit-reset-credits"
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == endpoint && int(calls.Load()) <= tc.failures {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
					return
				}
				if r.URL.Path == "/backend-api/wham/usage" {
					_, _ = w.Write([]byte(`{"rate_limit":null}`))
				} else {
					_, _ = w.Write([]byte(`{"available_count":2}`))
				}
			}))
			defer server.Close()
			factory := func(proxy string) (*req.Client, error) {
				client, err := newQuotaRedirectingFactory(server)(proxy)
				client.WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
					return func(r *req.Request) (*req.Response, error) {
						if r.URL.Path == endpoint {
							for _, state := range repo.observations {
								require.Equal(t, "ok", state.Kind, "no failure may be persisted before retries finish")
							}
							attempt := calls.Add(1)
							if tc.network && int(attempt) <= tc.failures {
								return nil, fmt.Errorf("proxy connection reset for secret")
							}
						}
						return rt.RoundTrip(r)
					}
				})
				return client, err
			}
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), factory, nil)
			usage, err := svc.QueryUsage(context.Background(), a.ID)
			require.EqualValues(t, tc.calls, calls.Load())
			if tc.wantKind == "" {
				require.NoError(t, err)
				require.Nil(t, usage.Warning)
				for _, state := range repo.observations {
					require.Equal(t, "ok", state.Kind)
				}
			} else {
				state := repo.observations[len(repo.observations)-1]
				require.Equal(t, tc.wantKind, state.Kind)
				require.NotContains(t, state.Message, "secret")
				if tc.network {
					require.Contains(t, state.Message, "proxy connection reset")
				}
				if tc.source == "reset_credits" {
					require.NoError(t, err)
					require.Equal(t, tc.wantKind, usage.Warning.Kind)
				} else {
					require.Error(t, err)
					require.Equal(t, tc.wantKind, openAIHealthErrorKind(err))
				}
			}
		})
	}
}
