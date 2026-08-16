//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type thresholdPauseReconcileRepoStub struct {
	rateLimitAccountRepoStub
	updated        bool
	reconcileCalls int
	expectedReason string
	nextUntil      *time.Time
	nextReason     string
	activeAccounts []Account
}

func (r *thresholdPauseReconcileRepoStub) ListActive(context.Context) ([]Account, error) {
	return r.activeAccounts, nil
}

func (r *thresholdPauseReconcileRepoStub) ReconcileAccountSchedulingThresholdPause(
	_ context.Context,
	_ int64,
	expectedReason string,
	until *time.Time,
	reason string,
) (bool, error) {
	r.reconcileCalls++
	r.expectedReason = expectedReason
	r.nextUntil = cloneTimePtr(until)
	r.nextReason = reason
	return r.updated, nil
}

type thresholdPauseCacheRecorder struct {
	deleted []int64
	set     map[int64]*TempUnschedState
}

func (c *thresholdPauseCacheRecorder) SetTempUnsched(_ context.Context, accountID int64, state *TempUnschedState) error {
	if c.set == nil {
		c.set = make(map[int64]*TempUnschedState)
	}
	c.set[accountID] = state
	return nil
}

func (c *thresholdPauseCacheRecorder) GetTempUnsched(context.Context, int64) (*TempUnschedState, error) {
	return nil, nil
}

func (c *thresholdPauseCacheRecorder) DeleteTempUnsched(_ context.Context, accountID int64) error {
	c.deleted = append(c.deleted, accountID)
	return nil
}

type thresholdPauseRuntimeBlocker struct {
	blocked []int64
	cleared []int64
}

func (b *thresholdPauseRuntimeBlocker) BlockAccountScheduling(account *Account, _ time.Time, _ string) {
	if account != nil {
		b.blocked = append(b.blocked, account.ID)
	}
}

func (b *thresholdPauseRuntimeBlocker) ClearAccountSchedulingBlock(accountID int64) {
	b.cleared = append(b.cleared, accountID)
}

func TestRateLimitService_ApplyAccountSchedulingThreshold_SetsTempUnschedulable(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})

	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":80}`

	accountRepo := &rateLimitAccountRepoStub{}
	rl := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	until := time.Now().UTC().Add(6 * time.Hour)
	account := &Account{
		ID:          1001,
		Platform:    PlatformOpenAI,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"codex_7d_used_percent": 91.5,
			"codex_7d_reset_at":     until.Format(time.RFC3339),
		},
	}

	blocked := rl.ApplyAccountSchedulingThreshold(context.Background(), account)

	require.True(t, blocked)
	require.Equal(t, 1, accountRepo.tempCalls)
	require.NotNil(t, account.TempUnschedulableUntil)
	require.WithinDuration(t, until, *account.TempUnschedulableUntil, time.Second)
	require.True(t, IsAccountSchedulingThresholdReason(accountRepo.lastTempReason))

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(accountRepo.lastTempReason), &payload))
	require.Equal(t, PlatformOpenAI, payload["platform"])
	require.Equal(t, "7d", payload["window"])
	require.Equal(t, float64(80), payload["threshold_percent"])
	require.Equal(t, float64(91.5), payload["used_percent"])
	require.Contains(t, payload["error_message"], "91.5% used >= 80%")
}

func TestRateLimitService_ApplyAccountSchedulingThreshold_UsesAccountOverrideInReason(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})

	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":90}`

	accountRepo := &rateLimitAccountRepoStub{}
	rl := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	until := time.Now().UTC().Add(6 * time.Hour)
	account := &Account{
		ID:          1003,
		Platform:    PlatformOpenAI,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"account_scheduling_threshold": 80,
		},
		Extra: map[string]any{
			"codex_7d_used_percent": 85.5,
			"codex_7d_reset_at":     until.Format(time.RFC3339),
		},
	}

	blocked := rl.ApplyAccountSchedulingThreshold(context.Background(), account)

	require.True(t, blocked)
	require.Equal(t, 1, accountRepo.tempCalls)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(accountRepo.lastTempReason), &payload))
	require.Equal(t, float64(80), payload["threshold_percent"])
	require.Equal(t, float64(85.5), payload["used_percent"])
	require.Contains(t, payload["error_message"], "85.5% used >= 80%")
}

func TestRateLimitService_ApplyAccountSchedulingThreshold_SkipsDuplicateTempUnschedulable(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})

	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":80}`

	accountRepo := &rateLimitAccountRepoStub{}
	rl := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	until := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)
	existingReason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform:         PlatformOpenAI,
		Window:           "7d",
		ThresholdPercent: 80,
		UsedPercent:      91.5,
		Until:            until,
		Now:              until.Add(-time.Hour),
	})
	account := &Account{
		ID:                      1002,
		Platform:                PlatformOpenAI,
		Status:                  StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: existingReason,
		Extra: map[string]any{
			"codex_7d_used_percent": 91.5,
			"codex_7d_reset_at":     until.Format(time.RFC3339),
		},
	}

	blocked := rl.ApplyAccountSchedulingThreshold(context.Background(), account)

	require.True(t, blocked)
	require.Equal(t, 0, accountRepo.tempCalls)
	require.Equal(t, existingReason, account.TempUnschedulableReason)
	require.NotNil(t, account.TempUnschedulableUntil)
	require.True(t, until.Equal(*account.TempUnschedulableUntil))
}

func TestRateLimitService_ApplyAccountSchedulingThreshold_UnsupportedPlatformDoesNotBlock(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})

	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":80}`

	accountRepo := &rateLimitAccountRepoStub{}
	rl := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	account := &Account{
		ID:          2002,
		Platform:    PlatformKiro,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"account_scheduling_threshold": 1,
		},
		Extra: map[string]any{
			"kiro_sched_utilization": 99.0,
			"kiro_sched_reset_at":    time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}

	blocked := rl.ApplyAccountSchedulingThreshold(context.Background(), account)

	require.False(t, blocked)
	require.Equal(t, 0, accountRepo.tempCalls)
	require.Nil(t, account.TempUnschedulableUntil)
	require.Empty(t, account.TempUnschedulableReason)
}

func TestRateLimitService_ReconcileAccountSchedulingThresholdPolicy_ClearsDisabledPauseAcrossAllLayers(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":97}`

	until := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	oldReason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "5h", ThresholdPercent: 97, UsedPercent: 99, Until: until, Now: time.Now().UTC(),
	})
	account := &Account{
		ID: 3001, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
		Credentials:            map[string]any{"account_scheduling_threshold": 100},
		Extra:                  map[string]any{"codex_5h_used_percent": 99.0, "codex_5h_reset_at": until.Format(time.RFC3339)},
		TempUnschedulableUntil: &until, TempUnschedulableReason: oldReason,
	}
	repo := &thresholdPauseReconcileRepoStub{updated: true}
	cache := &thresholdPauseCacheRecorder{}
	blocker := &thresholdPauseRuntimeBlocker{}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))
	rl.SetAccountRuntimeBlocker(blocker)

	err := rl.ReconcileAccountSchedulingThresholdPolicy(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, 1, repo.reconcileCalls)
	require.Equal(t, oldReason, repo.expectedReason)
	require.Nil(t, repo.nextUntil)
	require.Empty(t, repo.nextReason)
	require.Nil(t, account.TempUnschedulableUntil)
	require.Empty(t, account.TempUnschedulableReason)
	require.Equal(t, []int64{account.ID}, cache.deleted)
	require.Empty(t, cache.set)
	require.Equal(t, []int64{account.ID}, blocker.cleared)
	require.Empty(t, blocker.blocked)
}

func TestRateLimitService_ReconcileAccountSchedulingThresholdPolicy_RewritesChangedPolicy(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":97}`

	until := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	oldReason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "5h", ThresholdPercent: 97, UsedPercent: 99, Until: until, Now: time.Now().UTC(),
	})
	account := &Account{
		ID: 3002, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
		Credentials:            map[string]any{"account_scheduling_threshold": 80},
		Extra:                  map[string]any{"codex_5h_used_percent": 85.0, "codex_5h_reset_at": until.Format(time.RFC3339)},
		TempUnschedulableUntil: &until, TempUnschedulableReason: oldReason,
	}
	repo := &thresholdPauseReconcileRepoStub{updated: true}
	cache := &thresholdPauseCacheRecorder{}
	blocker := &thresholdPauseRuntimeBlocker{}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))
	rl.SetAccountRuntimeBlocker(blocker)

	err := rl.ReconcileAccountSchedulingThresholdPolicy(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, 1, repo.reconcileCalls)
	require.NotNil(t, repo.nextUntil)
	next, ok := parseTempUnschedReasonPayload(repo.nextReason)
	require.True(t, ok)
	require.Equal(t, 80, next.ThresholdPercent)
	require.Equal(t, 85.0, next.UsedPercent)
	require.Equal(t, repo.nextReason, account.TempUnschedulableReason)
	require.Equal(t, []int64{account.ID}, cache.deleted)
	require.Contains(t, cache.set, account.ID)
	require.Equal(t, []int64{account.ID}, blocker.cleared)
	require.Equal(t, []int64{account.ID}, blocker.blocked)
}

func TestRateLimitService_ReconcileAccountSchedulingThresholdPolicy_PreservesOtherAndConcurrentReasons(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":97}`

	t.Run("unrelated source", func(t *testing.T) {
		until := time.Now().UTC().Add(time.Hour)
		account := &Account{ID: 3003, Platform: PlatformOpenAI, Credentials: map[string]any{"account_scheduling_threshold": 100}, TempUnschedulableUntil: &until, TempUnschedulableReason: BuildTempUnschedReasonPayload("oauth_401", "unauthorized")}
		repo := &thresholdPauseReconcileRepoStub{updated: true}
		rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

		require.NoError(t, rl.ReconcileAccountSchedulingThresholdPolicy(context.Background(), account))
		require.Zero(t, repo.reconcileCalls)
		require.NotNil(t, account.TempUnschedulableUntil)
	})

	t.Run("compare and swap lost", func(t *testing.T) {
		until := time.Now().UTC().Add(time.Hour)
		oldReason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{Platform: PlatformOpenAI, Window: "5h", ThresholdPercent: 97, UsedPercent: 99, Until: until, Now: time.Now().UTC()})
		account := &Account{ID: 3004, Platform: PlatformOpenAI, Credentials: map[string]any{"account_scheduling_threshold": 100}, TempUnschedulableUntil: &until, TempUnschedulableReason: oldReason}
		repo := &thresholdPauseReconcileRepoStub{updated: false}
		cache := &thresholdPauseCacheRecorder{}
		blocker := &thresholdPauseRuntimeBlocker{}
		rl := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
		rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))
		rl.SetAccountRuntimeBlocker(blocker)

		require.NoError(t, rl.ReconcileAccountSchedulingThresholdPolicy(context.Background(), account))
		require.Equal(t, 1, repo.reconcileCalls)
		require.NotNil(t, account.TempUnschedulableUntil)
		require.Empty(t, cache.deleted)
		require.Empty(t, blocker.cleared)
	})
}

func TestRateLimitService_ReconcileActiveAccountSchedulingThresholdPolicies_ClearsPersistedPauseAfterDeploy(t *testing.T) {
	accountSchedulingThresholdsSF.Forget(SettingKeyAccountSchedulingThresholds)
	accountSchedulingThresholdsCache.Store(&cachedAccountSchedulingThresholds{})
	settingsRepo := newMockSettingRepo()
	settingsRepo.data[SettingKeyAccountSchedulingThresholds] = `{"openai":97}`

	now := time.Now().UTC()
	until := now.Add(5 * 24 * time.Hour).Truncate(time.Second)
	oldReason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "7d", ThresholdPercent: 97, UsedPercent: 97, Until: until, Now: now.Add(-time.Hour),
	})
	repo := &thresholdPauseReconcileRepoStub{
		updated: true,
		activeAccounts: []Account{
			{
				ID: 4001, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{accountSchedulingThresholdCredentialKey: 100},
				Extra: map[string]any{
					"codex_7d_used_percent": 97.0,
					"codex_7d_reset_at":     until.Format(time.RFC3339),
				},
				TempUnschedulableUntil: &until, TempUnschedulableReason: oldReason,
			},
			{
				ID: 4002, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
				TempUnschedulableUntil:  &until,
				TempUnschedulableReason: BuildTempUnschedReasonPayload("oauth_401", "unauthorized"),
			},
			{
				ID: 4003, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
				TempUnschedulableUntil: &until, TempUnschedulableReason: oldReason,
			},
		},
	}
	cache := &thresholdPauseCacheRecorder{}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	rl.SetSettingService(NewSettingService(settingsRepo, &config.Config{}))

	count, err := rl.ReconcileActiveAccountSchedulingThresholdPolicies(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, 1, repo.reconcileCalls)
	require.Equal(t, oldReason, repo.expectedReason)
	require.Nil(t, repo.nextUntil)
	require.Empty(t, repo.nextReason)
	require.Equal(t, []int64{4001}, cache.deleted)
}
