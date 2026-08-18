//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestAdminService_EnsureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			privacyCalls := 0
			svc := &adminServiceImpl{
				accountRepo: &mockAccountRepoForGemini{},
				privacyClientFactory: func(proxyURL string) (*req.Client, error) {
					privacyCalls++
					return nil, errors.New("factory failed")
				},
			}

			account := &Account{
				ID:       101,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-1",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			got := svc.EnsureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, PrivacyModeFailed, got)
			require.Equal(t, 1, privacyCalls)
		})
	}
}

func TestAdminService_EnsureOpenAIPrivacy_RetriesUnknownMode(t *testing.T) {
	privacyCalls := 0
	svc := &adminServiceImpl{
		accountRepo: &mockAccountRepoForGemini{},
		privacyClientFactory: func(proxyURL string) (*req.Client, error) {
			privacyCalls++
			return nil, errors.New("factory failed")
		},
	}

	account := &Account{
		ID:       303,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-3",
		},
		Extra: map[string]any{
			"privacy_mode": "legacy_unknown_mode",
		},
	}

	require.Equal(t, PrivacyModeFailed, svc.EnsureOpenAIPrivacy(context.Background(), account))
	require.Equal(t, 1, privacyCalls)
	require.Equal(t, PrivacyModeFailed, account.Extra["privacy_mode"])
}

func TestAdminService_EnsureOpenAIPrivacy_SkipsConfirmedSuccess(t *testing.T) {
	privacyCalls := 0
	svc := &adminServiceImpl{
		accountRepo: &mockAccountRepoForGemini{},
		privacyClientFactory: func(proxyURL string) (*req.Client, error) {
			privacyCalls++
			return nil, errors.New("must not be called")
		},
	}
	account := &Account{
		ID:          307,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "token-7"},
		Extra:       map[string]any{"privacy_mode": PrivacyModeTrainingOff},
	}

	require.Empty(t, svc.EnsureOpenAIPrivacy(context.Background(), account))
	require.Equal(t, 0, privacyCalls)
}

func TestAdminService_EnsureOpenAIPrivacy_DoesNotMaskPersistenceFailure(t *testing.T) {
	repo := &privacyUpdateErrorAccountRepo{err: errors.New("write failed")}
	svc := &adminServiceImpl{
		accountRepo: repo,
		privacyClientFactory: func(proxyURL string) (*req.Client, error) {
			return nil, errors.New("factory failed")
		},
	}
	account := &Account{
		ID:       304,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-4",
		},
	}

	require.Equal(t, PrivacyModeFailed, svc.EnsureOpenAIPrivacy(context.Background(), account))
	_, exists := account.Extra["privacy_mode"]
	require.False(t, exists, "in-memory state must not claim success when persistence fails")
}

type privacyUpdateErrorAccountRepo struct {
	mockAccountRepoForGemini
	err error
}

func (r *privacyUpdateErrorAccountRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	return r.err
}

func TestAdminService_CreateAccount_TriggersOpenAIPrivacyOnSnapshot(t *testing.T) {
	repo := &privacyCreateAccountRepo{updates: make(chan map[string]any, 1)}
	svc := &adminServiceImpl{
		accountRepo: repo,
		privacyClientFactory: func(proxyURL string) (*req.Client, error) {
			return nil, errors.New("factory failed")
		},
	}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "new-openai-account",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeOAuth,
		Credentials:          map[string]any{"access_token": "token-5"},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err)

	select {
	case updates := <-repo.updates:
		require.Equal(t, PrivacyModeFailed, updates["privacy_mode"])
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for automatic OpenAI privacy attempt")
	}
	_, exists := account.Extra["privacy_mode"]
	require.False(t, exists, "background task must not mutate the account returned to the handler")
}

func TestAdminService_CreateAccount_SkipsFallbackWhenCallerOwnsPrivacy(t *testing.T) {
	repo := &privacyCreateAccountRepo{updates: make(chan map[string]any, 1)}
	svc := &adminServiceImpl{
		accountRepo: repo,
		privacyClientFactory: func(proxyURL string) (*req.Client, error) {
			return nil, errors.New("must not be called")
		},
	}

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                      "handler-owned-privacy",
		Platform:                  PlatformOpenAI,
		Type:                      AccountTypeOAuth,
		Credentials:               map[string]any{"access_token": "token-6"},
		SkipDefaultGroupBind:      true,
		SkipAutomaticPrivacySetup: true,
	})
	require.NoError(t, err)

	select {
	case <-repo.updates:
		t.Fatal("service fallback must not run when the caller owns privacy setup")
	case <-time.After(100 * time.Millisecond):
	}
}

type privacyCreateAccountRepo struct {
	mockAccountRepoForGemini
	updates chan map[string]any
}

func (r *privacyCreateAccountRepo) Create(ctx context.Context, account *Account) error {
	account.ID = 306
	return nil
}

func (r *privacyCreateAccountRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	r.updates <- maps.Clone(updates)
	return nil
}

func TestTokenRefreshService_ensureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		TokenRefresh: config.TokenRefreshConfig{
			MaxRetries:          1,
			RetryBackoffSeconds: 0,
		},
	}

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			service := NewTokenRefreshService(&tokenRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)
			privacyCalls := 0
			service.SetPrivacyDeps(func(proxyURL string) (*req.Client, error) {
				privacyCalls++
				return nil, errors.New("factory failed")
			}, nil)

			account := &Account{
				ID:       202,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-2",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			service.ensureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, 1, privacyCalls)
		})
	}
}
