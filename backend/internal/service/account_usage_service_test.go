package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

	applyExtraToUsage(usage, extra, now)
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

func TestHideUnconfirmedCodexWindows(t *testing.T) {
	t.Parallel()
	legacyUsage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 100},
		SevenDay: &UsageProgress{Utilization: 50},
	}
	hideUnconfirmedCodexWindows(legacyUsage, map[string]any{
		codexWham5hWindowPresentKey: true,
		codexWham7dWindowPresentKey: true,
		codexWhamUsageUpdatedAtKey:  time.Now().Format(time.RFC3339),
	})
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
	applyExtraToUsage(usage, extra, time.Now())
	hideUnconfirmedCodexWindows(usage, extra)
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
	}
	applyExtraToUsage(usage, extra, time.Now())
	hideUnconfirmedCodexWindows(usage, extra)
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
			codexWhamUsageUpdatedAtKey: "2026-08-17T07:00:00.123456789Z",
		},
	}

	if _, err := svc.getOpenAIUsage(context.Background(), account, false); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if reconciler.calls != 1 {
		t.Fatalf("threshold reconciler calls = %d, want 1", reconciler.calls)
	}
	if reconciler.expectedWhamUpdatedAt != "2026-08-17T07:00:00.123456789Z" {
		t.Fatalf("expected WHAM generation = %q", reconciler.expectedWhamUpdatedAt)
	}
	if repo.clearTempCalls != 0 {
		t.Fatalf("legacy ClearTempUnschedulable calls = %d, want 0", repo.clearTempCalls)
	}
	if account.TempUnschedulableUntil != nil || account.TempUnschedulableReason != "" {
		t.Fatalf("expected in-memory scheduling pause to be cleared, got until=%v reason=%q", account.TempUnschedulableUntil, account.TempUnschedulableReason)
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
		openAIQuotaService:  NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokenCache, nil), newQuotaRedirectingFactory(server)),
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
		openAIQuotaService:  NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokenCache, nil), newQuotaRedirectingFactory(server)),
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

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
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
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
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
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}
