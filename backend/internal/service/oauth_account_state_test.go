//go:build unit

package service

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOAuthRotationPreservesReauthorization(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			a := &Account{ID: 41, Platform: platform, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "old", "refresh_token": "old-refresh"}}
			repo := &refreshAPIAccountRepo{account: a}
			executor := &refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "late", "refresh_token": "late-refresh"}, onRefresh: func() {
				latest := *a
				latest.Credentials = map[string]any{"access_token": "reauthorized", "refresh_token": "reauthorized-refresh"}
				repo.account = &latest
			}}
			result, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshIfNeeded(context.Background(), a, executor, time.Minute)
			require.NoError(t, err)
			require.False(t, result.Refreshed)
			require.Equal(t, "reauthorized", result.Account.GetCredential("access_token"))
			require.Equal(t, "reauthorized", repo.account.GetCredential("access_token"))
		})
	}
}

type oauthStateRepoStub struct {
	refreshAPIAccountRepo
	stateCalls int
}

func (r *oauthStateRepoStub) SaveOAuthAuthenticationState(ctx context.Context, expected *Account, state OpenAIUpstreamHealth, until time.Time) (bool, error) {
	r.stateCalls++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !reflect.DeepEqual(OAuthCredentialIdentity(r.account.Credentials), OAuthCredentialIdentity(expected.Credentials)) {
		return false, nil
	}
	if r.account.Extra == nil {
		r.account.Extra = map[string]any{}
	}
	key := OpenAIUpstreamHealthExtraKey
	if expected.Platform == PlatformAnthropic {
		key = ClaudeOAuthStateKey
	}
	r.account.Extra[key] = map[string]any{"kind": state.Kind, "message": state.Message}
	if state.Kind == "auth_invalid" {
		r.account.Status = StatusError
	} else {
		r.account.TempUnschedulableUntil = &until
	}
	return true, nil
}

func TestOAuthDelayedGatewayErrorDoesNotDisableNewCredentials(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			old := &Account{ID: 42, Platform: platform, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "old", "refresh_token": "old-refresh"}}
			latest := *old
			latest.Credentials = map[string]any{"access_token": "new", "refresh_token": "new-refresh"}
			repo := &oauthStateRepoStub{refreshAPIAccountRepo: refreshAPIAccountRepo{account: &latest}}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			gateway := &OpenAIGatewayService{accountRepo: repo, rateLimitService: limits}
			require.False(t, gateway.handleOpenAIAccountUpstreamError(context.Background(), old, http.StatusUnauthorized, nil, []byte(`{"error":{"code":"token_invalidated"}}`)))
			require.Equal(t, StatusActive, latest.Status)
			require.Nil(t, latest.TempUnschedulableUntil)
		})
	}
}

func TestOAuthClaude401RequestsRefreshBeforeExpiry(t *testing.T) {
	a := &Account{ID: 43, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "bad", "refresh_token": "refresh", "expires_at": time.Now().Add(7 * time.Hour).Format(time.RFC3339)}}
	repo := &oauthStateRepoStub{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
	limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	require.True(t, limits.HandleUpstreamError(context.Background(), a, 401, nil, []byte(`{"error":{"message":"invalid access token"}}`)))
	require.True(t, NewClaudeTokenRefresher(nil).NeedsRefresh(a, time.Minute))
	executor := &refreshAPIExecutorStub{needsRefresh: true, credentials: map[string]any{"access_token": "valid", "refresh_token": "rotated", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	provider := NewClaudeTokenProvider(repo, nil, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)
	token, err := provider.GetAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "valid", token)
	require.Equal(t, 1, executor.refreshCalls)
}

func TestOAuthAuthenticationFailureSurvivesClientCancellation(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			a := &Account{ID: 44, Platform: platform, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "rejected", "refresh_token": "refresh"}}
			repo := &oauthStateRepoStub{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
			limits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			handled, applied := limits.handleOAuthAuthenticationFailure(ctx, a, http.StatusUnauthorized, []byte(`{"error":{"message":"invalid access token"}}`))
			require.True(t, handled)
			require.True(t, applied, "observed upstream rejection must survive downstream disconnect")
			require.NotNil(t, a.TempUnschedulableUntil)
		})
	}
}

func TestOpenAITokenProviderStaleMissingRefreshTokenPreservesReauthorization(t *testing.T) {
	old := &Account{ID: 45, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "old", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}}
	latest := snapshotOAuthRefreshAccount(old)
	latest.Credentials = map[string]any{"access_token": "new", "refresh_token": "new-refresh"}
	repo := &oauthStateRepoStub{refreshAPIAccountRepo: refreshAPIAccountRepo{account: latest}}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	_, err := provider.GetAccessToken(context.Background(), old)
	require.Error(t, err)
	require.Equal(t, 1, repo.stateCalls)
	require.Equal(t, StatusActive, latest.Status)
}
