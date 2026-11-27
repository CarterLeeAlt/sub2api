package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh   chan map[string]any
	updateExtraErr  error
	snapshotApplied *bool
	rateLimitCh     chan time.Time
	clearTempCalls  int
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return r.updateExtraErr
}

func (r *accountUsageCodexProbeRepo) UpdateOpenAICodexWhamSnapshotIfNewer(_ context.Context, _ int64, _ string, updates map[string]any) (bool, error) {
	if err := r.UpdateExtra(context.Background(), 0, updates); err != nil {
		return false, err
	}
	if r.snapshotApplied != nil {
		return *r.snapshotApplied, nil
	}
	return true, nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.clearTempCalls++
	return nil
}

type accountUsageThresholdReconciler struct {
	calls                 int
	expectedWhamUpdatedAt string
	quota429Calls         int
	quota429WhamUpdatedAt string
}

func (r *accountUsageThresholdReconciler) ReconcileAccountSchedulingThresholdPolicy(_ context.Context, account *Account) error {
	return errors.New("reason-only reconciliation must not be used for WHAM recovery")
}

func (r *accountUsageThresholdReconciler) ReconcileAccountSchedulingThresholdPolicyIfSnapshotUnchanged(_ context.Context, account *Account, expectedWhamUpdatedAt string) error {
	r.calls++
	r.expectedWhamUpdatedAt = expectedWhamUpdatedAt
	account.TempUnschedulableUntil = nil
	account.TempUnschedulableReason = ""
	return nil
}

func (r *accountUsageThresholdReconciler) ReconcileOpenAICodexQuotaRateLimitIfSnapshotUnchanged(_ context.Context, account *Account, expectedWhamUpdatedAt string) error {
	r.quota429Calls++
	r.quota429WhamUpdatedAt = expectedWhamUpdatedAt
	account.RateLimitedAt = nil
	account.RateLimitResetAt = nil
	delete(account.Extra, OpenAICodexRateLimitStateExtraKey)
	return nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("non-OpenAI account should not refresh Codex usage")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexWhamUsageUpdatedAtKey: staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale wham snapshot to trigger refresh")
	}

	if !isOpenAICodexSnapshotStale(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexWhamUsageUpdatedAtKey: now.Format(time.RFC3339),
		},
	}, now) {
		t.Fatal("expected unversioned wham snapshot to refresh immediately")
	}
}

func TestOpenAIUsageTransientFailureUsesShortRetryCooldown(t *testing.T) {
	a := &Account{ID: 98765, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "workspace"}}
	repo := &sparkShadowUsageTestRepo{accounts: map[int64]*Account{a.ID: a}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "token"}}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
	}))
	defer server.Close()
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), openAIQuotaService: NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil)}
	usage, err := svc.getOpenAIUsage(context.Background(), a, false)
	require.NoError(t, err)
	require.NotEmpty(t, usage.Error)
	require.EqualValues(t, 2, calls.Load(), "the upstream read retries before becoming a cached failure")
	now := time.Now()
	require.False(t, svc.shouldProbeOpenAICodexSnapshot(a.ID, now.Add(30*time.Second)))
	require.True(t, svc.shouldProbeOpenAICodexSnapshot(a.ID, now.Add(90*time.Second)), "failure must not suppress refresh for ten minutes")
	usage, err = svc.getOpenAIUsage(context.Background(), a, true)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Equal(t, 25.0, usage.FiveHour.Utilization)
	require.EqualValues(t, 3, calls.Load())
}

func TestOpenAIUsageBatchKeepsWeeklyQuotaAfterResponseHeaders(t *testing.T) {
	observed := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	weeklyReset := 5*24*time.Hour + 2*time.Hour
	account := Account{ID: 731, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	account.Extra = buildCodexWhamWindowExtraUpdates(&OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
		PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 53, LimitWindowSeconds: 604800, ResetAfterSeconds: int64(weeklyReset.Seconds())},
	}}, observed, false)
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "5")
	headers.Set("x-codex-primary-reset-after-seconds", "84300")
	headers.Set("x-codex-primary-window-minutes", "1440")
	snapshot := ParseCodexRateLimitHeaders(headers)
	mergeAccountExtra(&account, buildCodexUsageExtraUpdates(snapshot, time.Now()))

	logs := &windowCostRepo{}
	svc := &AccountUsageService{accountRepo: &stubOpenAIAccountRepo{accounts: []Account{account}},
		usageLogRepo: logs, cache: NewUsageCache(), openAIQuotaService: &OpenAIQuotaService{}}
	svc.cache.openAIProbeCache.Store(account.ID, observed.Add(openAIProbeCacheTTL))
	batch, failures, _, err := svc.GetUsageBatch(t.Context(), []int64{account.ID}, false)
	require.NoError(t, err)
	require.Empty(t, failures)
	usage := batch[account.ID]
	require.Equal(t, "passive", usage.Source)
	require.Nil(t, usage.FiveHour)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 53.0, usage.SevenDay.Utilization)
	require.Equal(t, observed.Add(weeklyReset), *usage.SevenDay.ResetsAt)
	require.Equal(t, observed, *usage.UpdatedAt)
	require.Equal(t, []time.Time{observed.Add(weeklyReset - 7*24*time.Hour)}, logs.starts)
}

func TestOpenAIUsageRefreshTracksWeeklyQuotaChanges(t *testing.T) {
	account := &Account{ID: 732, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "workspace"},
		Extra: map[string]any{
			codexWhamPresenceSchemaKey:  codexWhamPresenceSchemaV1,
			codexWhamUsageUpdatedAtKey:  formatCodexWhamSnapshotGeneration(time.Now()),
			codexWham5hWindowPresentKey: true,
			codexWham7dWindowPresentKey: true,
			"codex_5h_used_percent":     5.0,
			"codex_7d_used_percent":     5.0,
		},
	}
	// The account starts with a pre-upgrade cache. Then the upstream changes
	// its weekly allowance in both directions and adds/removes a 5h window.
	responses := []string{
		`{"rate_limit":{"primary_window":{"used_percent":53,"limit_window_seconds":604800,"reset_after_seconds":420000}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":8,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_after_seconds":84000}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":64,"limit_window_seconds":604800,"reset_after_seconds":432000}}}`,
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(responses) {
			http.Error(w, "unexpected quota probe", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responses[index]))
	}))
	defer server.Close()
	repo := &sparkShadowUsageTestRepo{accounts: map[int64]*Account{account.ID: account}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "token"}}
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(),
		openAIQuotaService: NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil)}
	for i, want := range []float64{53, 5, 64} {
		usage, err := svc.GetUsage(t.Context(), account.ID, i > 0)
		require.NoError(t, err)
		require.Empty(t, usage.Error)
		require.Equal(t, "active", usage.Source)
		require.NotNil(t, usage.SevenDay)
		require.Equal(t, want, usage.SevenDay.Utilization)
		require.Equal(t, i == 1, usage.FiveHour != nil)
		cached, err := svc.GetUsage(t.Context(), account.ID)
		require.NoError(t, err)
		require.Equal(t, "passive", cached.Source)
		require.Equal(t, usage.UpdatedAt, cached.UpdatedAt)
		require.Equal(t, usage.SevenDay.Utilization, cached.SevenDay.Utilization)
		require.Equal(t, usage.SevenDay.ResetsAt, cached.SevenDay.ResetsAt)
		require.Equal(t, i == 1, cached.FiveHour != nil)
	}
	require.EqualValues(t, len(responses), calls.Load())
}

// TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2 外审第9轮 P1:spark 影子用量走
// QueryUsage(/wham/usage,与 WSv2 无关),staleness 不得被 WSv2 门控,否则首刷后窗口永久冻结。
func TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2(t *testing.T) {
	t.Parallel()

	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}
	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	freshAt := now.Add(-time.Minute).Format(time.RFC3339)
	parentID := int64(7001)

	// 影子无 WSv2,但首刷后窗口已存在;过期 wham 时间戳必须触发再刷新。
	shadowStale := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra: map[string]any{
			codexWhamUsageUpdatedAtKey: staleAt,
			codexWhamPresenceSchemaKey: codexWhamPresenceSchemaV1,
		},
	}
	if !shouldRefreshOpenAICodexSnapshot(shadowStale, usage, now) {
		t.Fatal("expected stale spark shadow (no WSv2) to trigger refresh")
	}

	// 影子时间戳仍新鲜→不刷(TTL 生效)。
	shadowFresh := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra: map[string]any{
			codexWhamUsageUpdatedAtKey: freshAt,
			codexWhamPresenceSchemaKey: codexWhamPresenceSchemaV1,
		},
	}
	if shouldRefreshOpenAICodexSnapshot(shadowFresh, usage, now) {
		t.Fatal("expected fresh spark shadow to skip refresh (TTL not elapsed)")
	}

	// 反向对照:普通账号也由 /wham/usage 判定窗口，不再受 WSv2 开关门控。
	normalNoWS := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexWhamUsageUpdatedAtKey: staleAt,
			codexWhamPresenceSchemaKey: codexWhamPresenceSchemaV1,
		},
	}
	if !shouldRefreshOpenAICodexSnapshot(normalNoWS, usage, now) {
		t.Fatal("expected stale non-WSv2 account to refresh authoritative wham usage")
	}
}

func TestApplyExtraToUsageHidesAuthoritativelyAbsentWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 99},
		SevenDay: &UsageProgress{Utilization: 99},
	}
	extra := map[string]any{
		codexWham5hWindowPresentKey: false,
		codexWham7dWindowPresentKey: true,
		"codex_5h_used_percent":     42.0, // stale pre-wham snapshot
		"codex_7d_used_percent":     92.0,
	}

	applyExtraToUsage(usage, extra, now, false)
	if usage.FiveHour != nil {
		t.Fatalf("authoritatively absent 5h window remained visible: %#v", usage.FiveHour)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 92 {
		t.Fatalf("present 7d window was not rebuilt: %#v", usage.SevenDay)
	}
}

func TestHasCodexWhamWindowPresenceBlocksLegacyFallback(t *testing.T) {
	t.Parallel()

	if hasCodexWhamWindowPresence(map[string]any{
		codexWham5hWindowPresentKey: false,
		codexWham7dWindowPresentKey: true,
	}) != true {
		t.Fatal("expected authoritative wham presence markers to be detected")
	}
	if hasCodexWhamWindowPresence(map[string]any{
		"codex_5h_used_percent": 100.0,
	}) {
		t.Fatal("legacy codex fields must not count as authoritative presence")
	}
}

func TestApplyExtraToUsageRequiresConfirmedWhamWindows(t *testing.T) {
	t.Parallel()
	legacyUsage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 100},
		SevenDay: &UsageProgress{Utilization: 50},
	}
	applyExtraToUsage(legacyUsage, map[string]any{
		codexWham5hWindowPresentKey: true,
		codexWham7dWindowPresentKey: true,
		codexWhamUsageUpdatedAtKey:  time.Now().Format(time.RFC3339),
	}, time.Now(), true)
	if legacyUsage.FiveHour != nil || legacyUsage.SevenDay != nil {
		t.Fatalf("legacy unversioned presence must be hidden pending wham refresh: %#v", legacyUsage)
	}

	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 50},
		SevenDay: &UsageProgress{Utilization: 75},
	}
	extra := map[string]any{
		codexWham5hWindowPresentKey: false,
		codexWhamPresenceSchemaKey:  codexWhamPresenceSchemaV1,
	}
	applyExtraToUsage(usage, extra, time.Now(), true)
	if usage.FiveHour != nil {
		t.Fatalf("known absent 5h window must remain hidden: %#v", usage.FiveHour)
	}
	if usage.SevenDay != nil {
		t.Fatalf("unknown 7d window must not render legacy data: %#v", usage.SevenDay)
	}

	usage = &UsageInfo{FiveHour: &UsageProgress{}, SevenDay: &UsageProgress{}}
	extra = map[string]any{
		codexWham5hWindowPresentKey: false,
		codexWham7dWindowPresentKey: true,
		codexWhamPresenceSchemaKey:  codexWhamPresenceSchemaV1,
		codexWham7dUsedPercentKey:   75.0,
		"codex_7d_reset_at":         time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	applyExtraToUsage(usage, extra, time.Now(), true)
	require.Nil(t, usage.SevenDay, "a row from before WHAM stored its own reset time must be refreshed")
	extra["codex_wham_7d_reset_at"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	applyExtraToUsage(usage, extra, time.Now(), true)
	if usage.FiveHour != nil || usage.SevenDay == nil {
		t.Fatalf("expected per-window authoritative visibility, got 5h=%#v 7d=%#v", usage.FiveHour, usage.SevenDay)
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
	if _, ok := updates[codexWham5hWindowPresentKey]; ok {
		t.Fatal("legacy response headers must not overwrite authoritative 5h presence")
	}
	if _, ok := updates[codexWham7dWindowPresentKey]; ok {
		t.Fatal("legacy response headers must not overwrite authoritative 7d presence")
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	persisted, err := svc.persistOpenAICodexProbeSnapshot(context.Background(), 321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("persistOpenAICodexProbeSnapshot() error = %v", err)
	}
	if !persisted {
		t.Fatal("persistOpenAICodexProbeSnapshot() persisted = false, want true")
	}

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	default:
		t.Fatal("persistOpenAICodexProbeSnapshot returned before updating extra")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotReturnsUpdateError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("database unavailable")
	repo := &accountUsageCodexProbeRepo{updateExtraErr: wantErr}
	svc := &AccountUsageService{accountRepo: repo}

	persisted, err := svc.persistOpenAICodexProbeSnapshot(context.Background(), 322, map[string]any{
		codexWhamUsageUpdatedAtKey: "2026-08-17T07:00:00.123456789Z",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("persistOpenAICodexProbeSnapshot() error = %v, want %v", err, wantErr)
	}
	if persisted {
		t.Fatal("persistOpenAICodexProbeSnapshot() persisted = true after repository error")
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_ClearsRecoveredSchedulingThresholdPause(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(5 * 24 * time.Hour)
	reason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform:         PlatformOpenAI,
		Window:           "7d",
		ThresholdPercent: 90,
		UsedPercent:      95,
		Until:            until,
		Now:              now.Add(-time.Hour),
	})
	repo := &accountUsageCodexProbeRepo{}
	reconciler := &accountUsageThresholdReconciler{}
	svc := &AccountUsageService{accountRepo: repo, thresholdReconciler: reconciler}
	whamGeneration := formatCodexWhamSnapshotGeneration(now)
	account := &Account{
		ID:                      3211,
		Platform:                PlatformOpenAI,
		Type:                    AccountTypeOAuth,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: reason,
		Extra: map[string]any{
			"codex_5h_used_percent":    0.0,
			"codex_5h_reset_at":        now.Add(2 * time.Hour).Format(time.RFC3339),
			"codex_7d_used_percent":    0.0,
			"codex_7d_reset_at":        until.Format(time.RFC3339),
			"codex_usage_updated_at":   now.Format(time.RFC3339),
			codexWhamUsageUpdatedAtKey: whamGeneration,
			codexWham7dUsedPercentKey:  0.0,
		},
	}

	if _, err := svc.getOpenAIUsage(context.Background(), account, false); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if reconciler.calls != 1 {
		t.Fatalf("threshold reconciler calls = %d, want 1", reconciler.calls)
	}
	if reconciler.expectedWhamUpdatedAt != whamGeneration {
		t.Fatalf("expected WHAM generation = %q, want %q", reconciler.expectedWhamUpdatedAt, whamGeneration)
	}
	if repo.clearTempCalls != 0 {
		t.Fatalf("legacy ClearTempUnschedulable calls = %d, want 0", repo.clearTempCalls)
	}
	if account.TempUnschedulableUntil != nil || account.TempUnschedulableReason != "" {
		t.Fatalf("expected in-memory scheduling pause to be cleared, got until=%v reason=%q", account.TempUnschedulableUntil, account.TempUnschedulableReason)
	}
}

func TestAccountUsageService_GetOpenAIUsageReconcilesRecoverableQuota429(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	observedAt := now.Add(-time.Minute)
	resetAt := now.Add(4 * time.Hour)
	whamGeneration := formatCodexWhamSnapshotGeneration(now)
	repo := &accountUsageCodexProbeRepo{}
	reconciler := &accountUsageThresholdReconciler{}
	svc := &AccountUsageService{accountRepo: repo, thresholdReconciler: reconciler}
	account := &Account{
		ID:               3213,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeOAuth,
		RateLimitedAt:    &observedAt,
		RateLimitResetAt: &resetAt,
		Extra: map[string]any{
			OpenAICodexRateLimitStateExtraKey: OpenAICodexRateLimitState{
				Version:          openAICodexRateLimitStateVersion,
				Source:           openAICodexQuota429Source,
				Window:           "5h",
				ObservedAt:       observedAt.Format(time.RFC3339Nano),
				ResetAt:          resetAt.Format(time.RFC3339Nano),
				UsedPercent:      100,
				ThresholdPercent: 95,
			},
			codexWhamPresenceSchemaKey:  codexWhamPresenceSchemaV1,
			codexWham5hWindowPresentKey: true,
			codexWham7dWindowPresentKey: false,
			codexWhamUsageUpdatedAtKey:  whamGeneration,
			"codex_usage_updated_at":    now.Format(time.RFC3339),
			"codex_5h_used_percent":     0.0,
			"codex_5h_reset_at":         resetAt.Format(time.RFC3339Nano),
		},
	}

	_, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if reconciler.quota429Calls != 1 || reconciler.quota429WhamUpdatedAt != whamGeneration {
		t.Fatalf("quota 429 reconciliation = (%d, %q), want (1, %q)", reconciler.quota429Calls, reconciler.quota429WhamUpdatedAt, whamGeneration)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("expected recovered quota 429 to clear in-memory state, got %v", account.RateLimitResetAt)
	}
}

func TestAccountUsageService_GetOpenAIUsageWithoutSnapshotReconcilerFailsClosed(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(5 * 24 * time.Hour)
	reason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "7d", ThresholdPercent: 90, UsedPercent: 95, Until: until, Now: now.Add(-time.Hour),
	})
	repo := &accountUsageCodexProbeRepo{}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		ID: 3212, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		TempUnschedulableUntil: &until, TempUnschedulableReason: reason,
		Extra: map[string]any{
			"codex_7d_used_percent":    0.0,
			"codex_7d_reset_at":        until.Format(time.RFC3339),
			"codex_usage_updated_at":   now.Format(time.RFC3339),
			codexWhamUsageUpdatedAtKey: "2026-08-17T07:00:00.987654321Z",
		},
	}

	if _, err := svc.getOpenAIUsage(context.Background(), account, false); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if repo.clearTempCalls != 0 {
		t.Fatalf("legacy ClearTempUnschedulable calls = %d, want 0", repo.clearTempCalls)
	}
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableReason == "" {
		t.Fatal("recovery without a generation-aware reconciler must preserve the pause")
	}
}

func TestAccountUsageService_GetOpenAIUsageMissingRateLimitDoesNotRecover(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(5 * 24 * time.Hour)
	reason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "7d", ThresholdPercent: 90, UsedPercent: 95, Until: until, Now: now.Add(-time.Hour),
	})
	account := Account{
		ID: 3213, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials:            map[string]any{"chatgpt_account_id": "org-incomplete-wham"},
		TempUnschedulableUntil: &until, TempUnschedulableReason: reason,
		Extra: map[string]any{
			"codex_7d_used_percent":    0.0,
			"codex_7d_reset_at":        until.Format(time.RFC3339),
			"codex_usage_updated_at":   now.Format(time.RFC3339),
			codexWhamUsageUpdatedAtKey: "2026-08-17T07:00:00.111111111Z",
			codexWhamPresenceSchemaKey: codexWhamPresenceSchemaV1,
		},
	}
	repo := &accountUsageCodexProbeRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
	}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(&account): "fake-token"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro"}`))
	}))
	defer server.Close()

	reconciler := &accountUsageThresholdReconciler{}
	svc := &AccountUsageService{
		accountRepo:         repo,
		openAIQuotaService:  NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokenCache, nil), newQuotaRedirectingFactory(server), nil),
		thresholdReconciler: reconciler,
	}

	if _, err := svc.getOpenAIUsage(context.Background(), &account, true); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if reconciler.calls != 0 {
		t.Fatalf("threshold reconciler calls = %d after incomplete WHAM response, want 0", reconciler.calls)
	}
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableReason == "" {
		t.Fatal("incomplete WHAM response must preserve the existing threshold pause")
	}
}

func TestAccountUsageService_GetOpenAIUsageStaleSnapshotWriteDoesNotRecover(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	until := now.Add(5 * 24 * time.Hour)
	reason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform: PlatformOpenAI, Window: "7d", ThresholdPercent: 90, UsedPercent: 95, Until: until, Now: now.Add(-time.Hour),
	})
	account := Account{
		ID: 3214, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials:            map[string]any{"chatgpt_account_id": "org-stale-wham"},
		TempUnschedulableUntil: &until, TempUnschedulableReason: reason,
		Extra: map[string]any{
			"codex_7d_used_percent":    0.0,
			"codex_7d_reset_at":        until.Format(time.RFC3339),
			"codex_usage_updated_at":   now.Format(time.RFC3339),
			codexWhamUsageUpdatedAtKey: "2026-08-17T07:00:00.111111111Z",
			codexWhamPresenceSchemaKey: codexWhamPresenceSchemaV1,
		},
	}
	notApplied := false
	repo := &accountUsageCodexProbeRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
		snapshotApplied:       &notApplied,
	}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(&account): "fake-token"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":null}`))
	}))
	defer server.Close()

	reconciler := &accountUsageThresholdReconciler{}
	svc := &AccountUsageService{
		accountRepo:         repo,
		openAIQuotaService:  NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokenCache, nil), newQuotaRedirectingFactory(server), nil),
		thresholdReconciler: reconciler,
	}

	if _, err := svc.getOpenAIUsage(context.Background(), &account, true); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if reconciler.calls != 0 {
		t.Fatalf("threshold reconciler calls = %d after stale snapshot write, want 0", reconciler.calls)
	}
	if account.TempUnschedulableUntil == nil || account.TempUnschedulableReason == "" {
		t.Fatal("out-of-order WHAM snapshot must not clear the existing threshold pause")
	}
	if got := codexWhamSnapshotGeneration(account.Extra); got != "2026-08-17T07:00:00.111111111Z" {
		t.Fatalf("in-memory WHAM generation changed after rejected write: %q", got)
	}
}

func TestAccountUsageService_GetOpenAIUsage_PreservesSnapshotObservationTime(t *testing.T) {
	t.Parallel()
	observedAt := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	for _, stamp := range []string{observedAt.Format(time.RFC3339), "", "invalid"} {
		t.Run(stamp, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					"codex_5h_used_percent":  10.0,
					"codex_7d_used_percent":  100.0,
					"codex_usage_updated_at": stamp,
				},
			}
			usage, err := (&AccountUsageService{}).getOpenAIUsage(context.Background(), account, false)
			if err != nil {
				t.Fatal(err)
			}
			if stamp == observedAt.Format(time.RFC3339) {
				if usage.UpdatedAt == nil || !usage.UpdatedAt.Equal(observedAt) {
					t.Fatalf("cached snapshot must keep its observation time %v, got %v", observedAt, usage.UpdatedAt)
				}
			} else if usage.UpdatedAt != nil {
				t.Fatalf("unknown observation time must not become the request time: %v", usage.UpdatedAt)
			}
			if usage.Source != "passive" || usage.Error != "" {
				t.Fatalf("snapshot read should be passive, not a failed refresh: %#v", usage)
			}
		})
	}
}

func TestAccountUsageService_GetOpenAIUsage_RefreshFailureKeepsCachedDataAndReportsError(t *testing.T) {
	t.Parallel()
	observedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	for _, shadow := range []bool{false, true} {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"codex_7d_used_percent":  100.0,
				"codex_usage_updated_at": observedAt.Format(time.RFC3339),
			},
		}
		if shadow {
			parentID := int64(10)
			account.ParentAccountID = &parentID
			account.QuotaDimension = QuotaDimensionSpark
		}
		// Ordinary accounts have no token; shadow accounts have no quota service.
		// Neither case may turn a failed query into a fresh, successful snapshot.
		usage, err := (&AccountUsageService{}).getOpenAIUsage(context.Background(), account, true)
		if err != nil {
			t.Fatal(err)
		}
		if usage.ErrorCode != "quota_refresh_failed" || usage.Error == "" {
			t.Fatalf("refresh failure was hidden (shadow=%v): %#v", shadow, usage)
		}
		if usage.SevenDay == nil || usage.SevenDay.Utilization != 100 {
			t.Fatalf("last known quota should remain available: %#v", usage.SevenDay)
		}
		if usage.UpdatedAt == nil || !usage.UpdatedAt.Equal(observedAt) || usage.Source != "passive" {
			t.Fatalf("failed refresh must not advance snapshot freshness: %#v", usage)
		}
		if account.RateLimitResetAt != nil || account.Extra["codex_usage_updated_at"] != observedAt.Format(time.RFC3339) {
			t.Fatal("failed refresh must not mutate the snapshot or scheduler state")
		}
	}
}

func TestAccountUsageService_GetOpenAIUsage_RefreshFailureWithoutSnapshot(t *testing.T) {
	t.Parallel()
	svc := &AccountUsageService{usageLogRepo: &usageBatchLogRepoStub{}}
	usage, err := svc.getOpenAIUsage(context.Background(), &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ErrorCode != "quota_refresh_failed" || usage.UpdatedAt != nil {
		t.Fatalf("missing upstream quota must remain unknown after failure: %#v", usage)
	}
	for name, window := range map[string]*UsageProgress{"5h": usage.FiveHour, "7d": usage.SevenDay} {
		if window == nil || window.WindowStats == nil || !window.UtilizationUnknown {
			t.Errorf("%s should keep local stats while marking quota unknown: %#v", name, window)
		}
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now, false)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now, false)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now, false)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}
