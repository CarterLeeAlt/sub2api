package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func catalogTestAccount() *Account {
	return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "token"}}
}

func TestModelCatalogCreationAndEligibility(t *testing.T) {
	for _, mode := range []string{"manual", "refresh-token", "mobile-refresh-token", "auth-json", "agent", "pat", "legacy-pat", "apikey", "setup-token", "shadow", "passthrough", "legacy-passthrough", "other"} {
		t.Run(mode, func(t *testing.T) {
			a := catalogTestAccount()
			eligible := true
			switch mode {
			case "refresh-token", "mobile-refresh-token":
				a.Credentials["refresh_token"] = "refresh-token"
			case "auth-json":
				a.Credentials["auth_mode"] = "chatgpt"
			case "apikey", "setup-token":
				a.Type = mode
				eligible = false
			case "agent":
				a.Credentials["auth_mode"] = OpenAIAuthModeAgentIdentity
			case "pat":
				a.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
			case "legacy-pat":
				a.Credentials["openai_auth_mode"] = "personal_access_token"
			case "shadow":
				eligible = false
				id := int64(2)
				a.ParentAccountID = &id
			case "passthrough":
				eligible = false
				a.Extra = map[string]any{"openai_passthrough": true}
			case "legacy-passthrough":
				eligible = false
				a.Extra = map[string]any{"openai_oauth_passthrough": true}
			case "other":
				eligible = false
				a.Platform = PlatformAnthropic
			}
			require.NoError(t, InitializeAccountModelCatalog(a))
			require.Equal(t, eligible, a.ModelCatalogPending())
			if eligible {
				require.False(t, a.IsSchedulable())
				require.True(t, a.Schedulable)
			} else {
				require.Empty(t, a.ModelCatalog().Source)
			}
		})
	}
}

func TestModelCatalogEditsPreserveLatestWhitelistAndAliases(t *testing.T) {
	current := catalogTestAccount()
	current.Credentials["model_mapping"] = map[string]any{"fresh": "fresh", "alias": "target"}
	SetAccountModelCatalog(current, AccountModelCatalog{Source: "upstream", Revision: "r1", Ready: true, SyncedAt: "2026-11-13T00:00:00Z"})
	stale := *current
	stale.Credentials = map[string]any{"access_token": "token", "model_mapping": map[string]any{"old": "old"}}
	require.NoError(t, MergeAccountModelCatalog(&stale, current))
	require.Equal(t, current.GetModelMapping(), stale.GetModelMapping())
	stale.Credentials = map[string]any{"model_mapping": map[string]any{"old": "old", "fresh": "manual-target", "new-alias": "target"}}
	stale.ModelCatalogEdit = &ModelCatalogEdit{Source: "upstream", Revision: "r1"}
	require.NoError(t, MergeAccountModelCatalog(&stale, current))
	require.Equal(t, map[string]string{"fresh": "manual-target", "new-alias": "target"}, stale.GetModelMapping())
	require.NotEqual(t, "r1", stale.ModelCatalog().Revision)
	require.NoError(t, MergeAccountModelCatalog(&stale, current))
	stale.ModelCatalogEdit.Revision = "stale"
	require.Error(t, MergeAccountModelCatalog(&stale, current))
	stale.ModelCatalogEdit = &ModelCatalogEdit{Source: "custom", Revision: "r1"}
	stale.Credentials = map[string]any{"model_mapping": map[string]any{"alias": "target"}}
	require.NoError(t, MergeAccountModelCatalog(&stale, current))
	require.Equal(t, "custom", stale.ModelCatalog().Source)
	require.Equal(t, map[string]string{"alias": "target"}, stale.GetModelMapping())
	stale.Type = AccountTypeAPIKey
	stale.ModelCatalogEdit = nil
	stale.Credentials = map[string]any{"api_key": "key", "model_mapping": map[string]any{"apikey-model": "apikey-model"}}
	require.NoError(t, MergeAccountModelCatalog(&stale, current))
	require.Equal(t, map[string]string{"apikey-model": "apikey-model"}, stale.GetModelMapping())
	require.Equal(t, "custom", stale.ModelCatalog().Source)
}

type catalogRefreshTestStore struct {
	accounts []Account
	saved    []*Account
	nextSync time.Time
}

func (r *catalogRefreshTestStore) SetModelCatalogWake(func()) {}
func (r *catalogRefreshTestStore) ListDueModelCatalogAccounts(context.Context, int64, int, bool) ([]Account, error) {
	return r.accounts, nil
}
func (r *catalogRefreshTestStore) FinishModelCatalogRound(_ context.Context, _ time.Time, next time.Time) error {
	r.nextSync = next
	return nil
}
func (r *catalogRefreshTestStore) SaveModelCatalogObservation(_ context.Context, a *Account, _, _ string, state AccountModelCatalog) (bool, error) {
	copy := *a
	SetAccountModelCatalog(&copy, state)
	r.saved = append(r.saved, &copy)
	return true, nil
}

type catalogRefreshTestReader func(context.Context, *Account) ([]string, error)

func (f catalogRefreshTestReader) fetchOpenAICodexManifestEntries(ctx context.Context, a, _ *Account) ([]openAICodexManifestModel, []byte, error) {
	ids, err := f(ctx, a)
	var entries []openAICodexManifestModel
	for _, id := range ids {
		entries = append(entries, openAICodexManifestModel{Slug: id})
	}
	return entries, nil, err
}

func TestModelCatalogRefreshResultsAndManualScheduling(t *testing.T) {
	for _, outcome := range []string{"success", "empty", "error"} {
		for _, ready := range []bool{false, true} {
			t.Run(outcome+map[bool]string{false: "-first", true: "-ready"}[ready], func(t *testing.T) {
				a := catalogTestAccount()
				a.Schedulable = false
				a.Credentials["model_mapping"] = map[string]any{"old": "old", "alias": "manual"}
				SetAccountModelCatalog(a, AccountModelCatalog{Source: "upstream", Revision: "r", Ready: ready})
				original := maps.Clone(a.GetModelMapping())
				store := &catalogRefreshTestStore{accounts: []Account{*a}}
				reader := catalogRefreshTestReader(func(ctx context.Context, a *Account) ([]string, error) {
					_, bounded := ctx.Deadline()
					require.True(t, bounded)
					switch outcome {
					case "error":
						return nil, errors.New("network failed")
					case "empty":
						return []string{" "}, nil
					default:
						return []string{"new", "new", "alias"}, nil
					}
				})
				worker := NewOpenAIModelCatalogRefreshService(store, reader, &fakeLeaderLockCache{})
				defer worker.Stop()
				before := time.Now()
				require.NoError(t, worker.RunOnce(context.Background()))
				require.Len(t, store.saved, 1)
				got := store.saved[0]
				require.False(t, got.Schedulable)
				require.False(t, got.IsSchedulable())
				require.False(t, store.nextSync.Before(before.Add(30*time.Minute)))
				require.True(t, store.nextSync.Before(time.Now().Add(45*time.Minute+time.Second)))
				if outcome == "success" {
					require.Equal(t, map[string]string{"new": "new", "alias": "manual"}, got.GetModelMapping())
					require.True(t, got.ModelCatalog().Ready)
					require.NotEmpty(t, got.ModelCatalog().SyncedAt)
				} else {
					require.Equal(t, original, got.GetModelMapping())
					require.Equal(t, ready, got.ModelCatalog().Ready)
					require.NotEmpty(t, got.ModelCatalog().Error)
					next, err := time.Parse(time.RFC3339Nano, got.ModelCatalog().NextSyncAt)
					require.NoError(t, err)
					require.WithinDuration(t, before.Add(2*time.Minute), next, time.Second)
				}
			})
		}
	}
}

func TestModelCatalogRefreshSerialAcrossInstances(t *testing.T) {
	a := catalogTestAccount()
	require.NoError(t, InitializeAccountModelCatalog(a))
	b := *a
	b.ID = 2
	store := &catalogRefreshTestStore{accounts: []Account{*a, b}}
	locks := &fakeLeaderLockCache{}
	entered, release := make(chan int64, 2), make(chan struct{})
	reader := catalogRefreshTestReader(func(ctx context.Context, a *Account) ([]string, error) {
		entered <- a.ID
		select {
		case <-release:
			return []string{"model"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	first := NewOpenAIModelCatalogRefreshService(store, reader, locks)
	second := NewOpenAIModelCatalogRefreshService(store, reader, locks)
	defer first.Stop()
	defer second.Stop()
	done := make(chan error, 1)
	go func() { done <- first.RunOnce(context.Background()) }()
	require.Equal(t, int64(1), <-entered)
	require.NoError(t, second.RunOnce(context.Background()))
	require.Empty(t, entered)
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, int64(2), <-entered)
	require.Len(t, store.saved, 2)
}

func TestModelCatalogRefreshHTTPFailureSchedule(t *testing.T) {
	for _, status := range []int{401, 403, 404, 408, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := catalogTestAccount()
			SetAccountModelCatalog(a, AccountModelCatalog{Source: "upstream", Revision: "r"})
			store := &catalogRefreshTestStore{accounts: []Account{*a}}
			reader := catalogRefreshTestReader(func(context.Context, *Account) ([]string, error) {
				return nil, &UpstreamModelSyncError{Kind: UpstreamModelSyncErrorUpstream, StatusCode: status, Message: "upstream failed"}
			})
			worker := NewOpenAIModelCatalogRefreshService(store, reader, &fakeLeaderLockCache{})
			defer worker.Stop()
			before := time.Now()
			require.NoError(t, worker.RunOnce(context.Background()))
			require.Len(t, store.saved, 1)
			next, err := time.Parse(time.RFC3339Nano, store.saved[0].ModelCatalog().NextSyncAt)
			require.NoError(t, err)
			if status == 401 || status == 403 || status == 404 {
				require.False(t, next.Before(before.Add(30*time.Minute)))
			} else {
				require.WithinDuration(t, before.Add(2*time.Minute), next, time.Second)
			}
		})
	}
}
