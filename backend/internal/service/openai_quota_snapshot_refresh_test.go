package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type quotaSnapshotRefreshRepoStub struct {
	mu sync.Mutex

	accounts  []Account
	shadows   map[int64][]*Account
	listCalls int
	listErr   error
	shadowErr error

	canonicals map[int64]*Account
	getByIDErr error

	wham            map[int64]map[string]any
	whamGeneration  map[int64]string
	reset           map[int64]*OpenAIResetCreditSnapshot
	resetGeneration map[int64]string
}

func (r *quotaSnapshotRefreshRepoStub) ListWithFilters(
	_ context.Context,
	params pagination.PaginationParams,
	platform, accountType, status, search string,
	groupID int64,
	privacyMode string,
) ([]Account, *pagination.PaginationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listErr != nil {
		return nil, nil, r.listErr
	}
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth || status != "" || search != "" || groupID != 0 || privacyMode != "" {
		return nil, nil, errors.New("unexpected account scan filters")
	}
	start := params.Offset()
	if start >= len(r.accounts) {
		return nil, &pagination.PaginationResult{Page: params.Page, PageSize: params.PageSize, Pages: 1}, nil
	}
	end := min(start+params.Limit(), len(r.accounts))
	pages := (len(r.accounts) + params.Limit() - 1) / params.Limit()
	out := append([]Account(nil), r.accounts[start:end]...)
	return out, &pagination.PaginationResult{
		Total: int64(len(r.accounts)), Page: params.Page, PageSize: params.PageSize, Pages: pages,
	}, nil
}

func (r *quotaSnapshotRefreshRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	if canonical, ok := r.canonicals[id]; ok {
		clone := *canonical
		return &clone, nil
	}
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			clone := r.accounts[i]
			return &clone, nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *quotaSnapshotRefreshRepoStub) ListShadowsByParent(_ context.Context, parentID int64) ([]*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shadowErr != nil {
		return nil, r.shadowErr
	}
	return append([]*Account(nil), r.shadows[parentID]...), nil
}

func (r *quotaSnapshotRefreshRepoStub) UpdateOpenAICodexWhamSnapshotIfNewer(
	_ context.Context,
	accountID int64,
	generation string,
	updates map[string]any,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.whamGeneration[accountID]; current != "" && current > generation {
		return false, nil
	}
	if r.wham == nil {
		r.wham = make(map[int64]map[string]any)
	}
	if r.whamGeneration == nil {
		r.whamGeneration = make(map[int64]string)
	}
	r.whamGeneration[accountID] = generation
	r.wham[accountID] = cloneQuotaSnapshotMap(updates)
	return true, nil
}

func (r *quotaSnapshotRefreshRepoStub) UpdateOpenAIResetCreditSnapshotIfNewer(
	_ context.Context,
	accountID int64,
	generation string,
	snapshot *OpenAIResetCreditSnapshot,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.resetGeneration[accountID]; current != "" && current >= generation {
		return false, nil
	}
	if r.reset == nil {
		r.reset = make(map[int64]*OpenAIResetCreditSnapshot)
	}
	if r.resetGeneration == nil {
		r.resetGeneration = make(map[int64]string)
	}
	copySnapshot := *snapshot
	copySnapshot.Credits = append([]OpenAIRateLimitResetCreditDetail(nil), snapshot.Credits...)
	r.resetGeneration[accountID] = generation
	r.reset[accountID] = &copySnapshot
	return true, nil
}

func cloneQuotaSnapshotMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

type quotaSnapshotUsageReaderStub struct {
	mu        sync.Mutex
	usage     *OpenAIQuotaUsage
	errors    map[int64]error
	failFirst map[int64]int
	delay     time.Duration
	calls     []int64
	active    int
	maxActive int
}

func (q *quotaSnapshotUsageReaderStub) QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error) {
	q.mu.Lock()
	q.calls = append(q.calls, accountID)
	q.active++
	if q.active > q.maxActive {
		q.maxActive = q.active
	}
	if n := q.failFirst[accountID]; n > 0 {
		q.failFirst[accountID] = n - 1
		q.mu.Unlock()

		defer func() {
			q.mu.Lock()
			q.active--
			q.mu.Unlock()
		}()
		if q.delay > 0 {
			timer := time.NewTimer(q.delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		return nil, errors.New("transient upstream failure")
	}
	q.mu.Unlock()

	defer func() {
		q.mu.Lock()
		q.active--
		q.mu.Unlock()
	}()
	if q.delay > 0 {
		timer := time.NewTimer(q.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := q.errors[accountID]; err != nil {
		return nil, err
	}
	return q.usage, nil
}

type quotaSnapshotLeaderLockStub struct {
	acquired bool
	err      error
	calls    int
	releases int

	renewOK  bool
	renewErr error
	renews   int
}

func (l *quotaSnapshotLeaderLockStub) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	l.calls++
	return l.acquired, l.err
}

func (l *quotaSnapshotLeaderLockStub) RenewLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	l.renews++
	if l.renewErr != nil {
		return false, l.renewErr
	}
	return l.renewOK, nil
}

func (l *quotaSnapshotLeaderLockStub) ReleaseLeaderLock(context.Context, string, string) error {
	l.releases++
	return nil
}

// quotaSnapshotRecoveryReconcilerStub records the CAS-guarded recovery calls
// issued by the periodic refresher and can simulate a successful recovery by
// clearing the 429 provenance on the canonical account.
type quotaSnapshotRecoveryReconcilerStub struct {
	mu             sync.Mutex
	thresholdCalls []string
	quota429Calls  []string
	thresholdErr   error
	quota429Err    error
	clearOn429     bool
}

func (r *quotaSnapshotRecoveryReconcilerStub) ReconcileAccountSchedulingThresholdPolicyIfSnapshotUnchanged(
	_ context.Context, _ *Account, expectedWhamUpdatedAt string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.thresholdCalls = append(r.thresholdCalls, expectedWhamUpdatedAt)
	return r.thresholdErr
}

func (r *quotaSnapshotRecoveryReconcilerStub) ReconcileOpenAICodexQuotaRateLimitIfSnapshotUnchanged(
	_ context.Context, account *Account, expectedWhamUpdatedAt string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quota429Calls = append(r.quota429Calls, expectedWhamUpdatedAt)
	if r.clearOn429 && account != nil {
		account.RateLimitedAt = nil
		account.RateLimitResetAt = nil
		delete(account.Extra, OpenAICodexRateLimitStateExtraKey)
	}
	return r.quota429Err
}

func quotaSnapshotTestUsage() *OpenAIQuotaUsage {
	return &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow:   &OpenAIRateLimitWindow{UsedPercent: 25, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 120},
			SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 40, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 240},
		},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{{
			MeteredFeature: "codex_bengalfox",
			RateLimit: &OpenAIRateLimit{
				PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 75, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 60},
			},
		}},
		RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 2},
	}
}

func quotaSnapshotParent(id int64, status string, schedulable bool) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: status, Schedulable: schedulable,
		Credentials: map[string]any{"access_token": "test"},
	}
}

func TestOpenAIQuotaSnapshotRefreshScansAllParentStatusesAndPaginates(t *testing.T) {
	accounts := make([]Account, 0, 103)
	accounts = append(accounts,
		quotaSnapshotParent(1, StatusActive, true),
		quotaSnapshotParent(2, StatusDisabled, false),
		quotaSnapshotParent(3, StatusError, false),
	)
	for id := int64(4); id <= 101; id++ {
		accounts = append(accounts, quotaSnapshotParent(id, StatusActive, false))
	}
	parentID := int64(1)
	accounts = append(accounts,
		Account{ID: 102, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, QuotaDimension: QuotaDimensionSpark},
		Account{ID: 103, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	)
	repo := &quotaSnapshotRefreshRepoStub{accounts: accounts}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC) }

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, 2, repo.listCalls)
	require.Len(t, quota.calls, 101)
	require.Contains(t, quota.calls, int64(2), "schedulable=false inactive accounts must still refresh")
	require.Contains(t, quota.calls, int64(3), "error accounts must still refresh")
	require.NotContains(t, quota.calls, int64(102), "shadow rows must not query upstream")
	require.NotContains(t, quota.calls, int64(103), "API-key rows must not query OAuth quota")
}

func TestOpenAIQuotaSnapshotRefreshDistributesParentSparkAndCountOnlySnapshot(t *testing.T) {
	parent := quotaSnapshotParent(10, StatusDisabled, false)
	parentID := parent.ID
	shadow := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, QuotaDimension: QuotaDimensionSpark}
	repo := &quotaSnapshotRefreshRepoStub{
		accounts: []Account{parent},
		shadows:  map[int64][]*Account{parent.ID: {shadow}},
	}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 123456789, time.UTC) }

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, []int64{parent.ID}, quota.calls)
	require.Equal(t, float64(25), repo.wham[parent.ID]["codex_5h_used_percent"])
	require.Equal(t, float64(40), repo.wham[parent.ID]["codex_7d_used_percent"])
	require.Equal(t, float64(75), repo.wham[shadow.ID]["codex_5h_used_percent"])
	require.Equal(t, 2, repo.reset[parent.ID].AvailableCount)
	require.Empty(t, repo.reset[parent.ID].Credits)
	require.Equal(t, repo.reset[parent.ID], repo.reset[shadow.ID])
	require.Equal(t, "2026-08-25T10:00:00.123456789Z", repo.reset[parent.ID].FetchedAt)
	require.Equal(t, repo.whamGeneration[parent.ID], repo.resetGeneration[parent.ID])
}

func TestOpenAIQuotaSnapshotRefreshDelayCoversTenThroughFifteenMinutes(t *testing.T) {
	for index := 0; index < 6; index++ {
		require.Equal(t, time.Duration(10+index)*time.Minute, openAIQuotaSnapshotRefreshDelay(index))
	}
	for i := 0; i < 100; i++ {
		delay := randomOpenAIQuotaSnapshotRefreshDelay()
		require.GreaterOrEqual(t, delay, 10*time.Minute)
		require.LessOrEqual(t, delay, 15*time.Minute)
		require.Zero(t, delay%time.Minute)
	}
}

func TestOpenAIQuotaSnapshotRefreshLeaderLockSkipsBusyAndErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		lock    *quotaSnapshotLeaderLockStub
		wantErr bool
	}{
		{name: "held by peer", lock: &quotaSnapshotLeaderLockStub{acquired: false}},
		{name: "lock service error", lock: &quotaSnapshotLeaderLockStub{err: errors.New("redis unavailable")}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &quotaSnapshotRefreshRepoStub{accounts: []Account{quotaSnapshotParent(1, StatusActive, true)}}
			quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
			svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
			svc.SetLeaderLock(tt.lock, nil)

			err := svc.RunOnce(context.Background())
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, quota.calls)
			require.Equal(t, 1, tt.lock.calls)
			require.Zero(t, tt.lock.releases)
		})
	}
}

func TestOpenAIQuotaSnapshotRefreshRunsSerially(t *testing.T) {
	accounts := make([]Account, 12)
	for i := range accounts {
		accounts[i] = quotaSnapshotParent(int64(i+1), StatusActive, true)
	}
	repo := &quotaSnapshotRefreshRepoStub{accounts: accounts}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage(), delay: 25 * time.Millisecond}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, 1, quota.maxActive, "quota checks must run one account at a time")
	require.Len(t, quota.calls, len(accounts))
}

func TestOpenAIQuotaSnapshotRefreshRejectsOlderGeneration(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)

	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 2, time.UTC) }
	require.NoError(t, svc.refreshAccount(context.Background(), &account))
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC) }
	require.NoError(t, svc.refreshAccount(context.Background(), &account))

	require.Equal(t, "2026-08-25T10:00:00.000000002Z", repo.whamGeneration[account.ID])
	require.Equal(t, "2026-08-25T10:00:00.000000002Z", repo.resetGeneration[account.ID])
}

func TestOpenAIQuotaSnapshotRefreshFailurePreservesSnapshots(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	previous := &OpenAIResetCreditSnapshot{AvailableCount: 3, FetchedAt: "2026-08-25T09:00:00.000000000Z"}
	repo := &quotaSnapshotRefreshRepoStub{
		accounts: []Account{account},
		wham:     map[int64]map[string]any{account.ID: {"codex_5h_used_percent": float64(42)}},
		reset:    map[int64]*OpenAIResetCreditSnapshot{account.ID: previous},
	}
	quota := &quotaSnapshotUsageReaderStub{errors: map[int64]error{account.ID: errors.New("upstream unavailable")}}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.retryWait = func(context.Context, time.Duration) error { return nil }

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, float64(42), repo.wham[account.ID]["codex_5h_used_percent"])
	require.Same(t, previous, repo.reset[account.ID])
}

func TestOpenAIQuotaSnapshotRefreshAppliesPerAccountTimeout(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{accounts: []Account{account}}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage(), delay: time.Minute}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.accountTimeout = 10 * time.Millisecond
	svc.retryWait = func(context.Context, time.Duration) error { return nil }

	started := time.Now()
	require.NoError(t, svc.RunOnce(context.Background()))
	require.Less(t, time.Since(started), time.Second)
	require.Empty(t, repo.wham)
	require.Empty(t, repo.reset)
}

func TestOpenAIQuotaSnapshotRefreshRetriesOnceAfterDelay(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{accounts: []Account{account}}
	quota := &quotaSnapshotUsageReaderStub{
		usage:     quotaSnapshotTestUsage(),
		failFirst: map[int64]int{account.ID: 1},
	}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC) }

	var waits []time.Duration
	svc.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, []int64{account.ID, account.ID}, quota.calls, "exactly one retry must follow the failed first attempt")
	require.Equal(t, []time.Duration{openAIQuotaSnapshotRefreshRetryDelay}, waits)
	require.Equal(t, float64(25), repo.wham[account.ID]["codex_5h_used_percent"])
	require.Equal(t, 2, repo.reset[account.ID].AvailableCount)
}

func TestOpenAIQuotaSnapshotRefreshRetryGivesUpAfterSecondFailure(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	previous := &OpenAIResetCreditSnapshot{AvailableCount: 3, FetchedAt: "2026-08-25T09:00:00.000000000Z"}
	repo := &quotaSnapshotRefreshRepoStub{
		accounts: []Account{account},
		reset:    map[int64]*OpenAIResetCreditSnapshot{account.ID: previous},
	}
	quota := &quotaSnapshotUsageReaderStub{errors: map[int64]error{account.ID: errors.New("upstream unavailable")}}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)

	var waits []time.Duration
	svc.retryWait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, []int64{account.ID, account.ID}, quota.calls, "retry exactly once, never a third attempt")
	require.Equal(t, []time.Duration{openAIQuotaSnapshotRefreshRetryDelay}, waits)
	require.Same(t, previous, repo.reset[account.ID], "the last successful snapshot must survive both failures")
}

func TestOpenAIQuotaSnapshotRefreshRetryAbandonedWhenContextCanceled(t *testing.T) {
	account := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{accounts: []Account{account}}
	quota := &quotaSnapshotUsageReaderStub{errors: map[int64]error{account.ID: errors.New("upstream unavailable")}}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)

	ctx, cancel := context.WithCancel(context.Background())
	svc.retryWait = func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		return waitCtx.Err()
	}

	require.NoError(t, svc.RunOnce(ctx))
	require.Equal(t, []int64{account.ID}, quota.calls, "no retry after the wait is canceled")
	require.Empty(t, repo.reset)
}

// quotaSnapshotRecoveryCanonical builds a canonical account row carrying both a
// scheduling-threshold pause and a quota-derived 429, as the recovery trigger
// would observe after reloading the account from the database.
func quotaSnapshotRecoveryCanonical(accountID int64, refreshedAt time.Time) *Account {
	until := refreshedAt.Add(5 * 24 * time.Hour)
	reason := BuildDetailedAccountSchedulingThresholdReason(AccountSchedulingThresholdReasonInput{
		Platform:         PlatformOpenAI,
		Window:           "7d",
		ThresholdPercent: 90,
		UsedPercent:      95,
		Until:            until,
		Now:              refreshedAt.Add(-time.Hour),
	})
	observedAt := refreshedAt.Add(-time.Minute)
	resetAt := refreshedAt.Add(4 * time.Hour)
	var quota429State any
	_ = json.Unmarshal([]byte(buildOpenAICodexQuota429State("5h", observedAt, resetAt, 100, 95)), &quota429State)
	return &Account{
		ID:                      accountID,
		Platform:                PlatformOpenAI,
		Type:                    AccountTypeOAuth,
		Status:                  StatusActive,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: reason,
		RateLimitedAt:           &observedAt,
		RateLimitResetAt:        &resetAt,
		Extra: map[string]any{
			OpenAICodexRateLimitStateExtraKey: quota429State,
			"codex_usage_updated_at":          refreshedAt.Format(time.RFC3339),
			"codex_7d_used_percent":           0.0,
			"codex_7d_reset_at":               until.Format(time.RFC3339),
			// 恢复判定只认 WHAM 权威数据：刷新刚落库的代际与专写百分比。
			codexWhamUsageUpdatedAtKey: refreshedAt.UTC().Format(codexWhamGenerationLayout),
			codexWham7dUsedPercentKey:  0.0,
		},
	}
}

// A freshly persisted authoritative WHAM snapshot must trigger the same
// CAS-guarded recovery as the admin usage path, even with zero query traffic
// (gateway-only deployments). Both the scheduling-threshold pause and the
// quota-derived 429 must be reconciled against the generation that was just
// written, never against an in-memory copy.
func TestOpenAIQuotaSnapshotRefreshRecoversPauseAndQuota429AfterPersist(t *testing.T) {
	refreshedAt := time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC)
	parent := quotaSnapshotParent(1, StatusActive, true)
	canonical := quotaSnapshotRecoveryCanonical(parent.ID, refreshedAt)
	repo := &quotaSnapshotRefreshRepoStub{
		accounts:   []Account{parent},
		canonicals: map[int64]*Account{parent.ID: canonical},
	}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	reconciler := &quotaSnapshotRecoveryReconcilerStub{clearOn429: true}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.SetRecoveryReconciler(reconciler)
	svc.now = func() time.Time { return refreshedAt }

	require.NoError(t, svc.RunOnce(context.Background()))

	persistedGeneration := repo.whamGeneration[parent.ID]
	require.Equal(t, "2026-08-25T10:00:00.000000001Z", persistedGeneration)
	require.Equal(t, []string{persistedGeneration}, reconciler.quota429Calls,
		"429 recovery must use the exact generation persisted by this cycle")
	require.Equal(t, []string{persistedGeneration}, reconciler.thresholdCalls,
		"scheduling-threshold recovery must use the exact generation persisted by this cycle")
	// The stub clears the 429 provenance on the reloaded canonical row; the
	// state lives in the shared Extra map owned by the repo stub.
	require.NotContains(t, canonical.Extra, OpenAICodexRateLimitStateExtraKey,
		"429 provenance must be cleared by the recovery trigger")
}

// When a newer generation already won the CAS (e.g. a concurrent admin query
// refreshed first), the periodic refresher must not fire recovery: its expected
// generation no longer matches the database row.
func TestOpenAIQuotaSnapshotRefreshSkipsRecoveryWhenGenerationNotAdvanced(t *testing.T) {
	parent := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{
		accounts:       []Account{parent},
		whamGeneration: map[int64]string{parent.ID: "2026-08-25T11:00:00.000000000Z"},
	}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	reconciler := &quotaSnapshotRecoveryReconcilerStub{}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.SetRecoveryReconciler(reconciler)
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC) }

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Empty(t, reconciler.quota429Calls)
	require.Empty(t, reconciler.thresholdCalls)
	require.Equal(t, "2026-08-25T11:00:00.000000000Z", repo.whamGeneration[parent.ID],
		"the older generation must not overwrite the persisted one")
}

// A failing recovery reconcile must never interrupt the refresh cycle, matching
// the per-account failure handling of the query path.
func TestOpenAIQuotaSnapshotRefreshReconcileFailureDoesNotAbortCycle(t *testing.T) {
	parent := quotaSnapshotParent(1, StatusActive, true)
	repo := &quotaSnapshotRefreshRepoStub{
		accounts:   []Account{parent},
		canonicals: map[int64]*Account{parent.ID: quotaSnapshotRecoveryCanonical(parent.ID, time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC))},
	}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	reconciler := &quotaSnapshotRecoveryReconcilerStub{
		thresholdErr: errors.New("reconcile repository unavailable"),
		quota429Err:  errors.New("reconcile repository unavailable"),
	}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.SetRecoveryReconciler(reconciler)
	svc.now = func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 1, time.UTC) }

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Len(t, reconciler.quota429Calls, 1)
	require.Len(t, reconciler.thresholdCalls, 1)
	require.Equal(t, float64(25), repo.wham[parent.ID]["codex_5h_used_percent"],
		"the snapshot must still be persisted when recovery fails")
}

// Every processed page must renew the leader lock so a serial sweep that
// outlives the TTL cannot silently lose leadership mid-cycle.
func TestOpenAIQuotaSnapshotRefreshRenewsLeaderLockPerPage(t *testing.T) {
	accounts := make([]Account, 0, 101)
	for id := int64(1); id <= 101; id++ {
		accounts = append(accounts, quotaSnapshotParent(id, StatusActive, true))
	}
	repo := &quotaSnapshotRefreshRepoStub{accounts: accounts}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	lock := &quotaSnapshotLeaderLockStub{acquired: true, renewOK: true}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.SetLeaderLock(lock, nil)

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, 2, lock.renews, "one renewal per processed page")
	require.Len(t, quota.calls, 101)
	require.Equal(t, 1, lock.releases)
}

// Losing the leader lock at a page boundary must abort the cycle immediately:
// continuing would sweep concurrently with the new leader.
func TestOpenAIQuotaSnapshotRefreshAbortsWhenLeaderLockRenewFails(t *testing.T) {
	accounts := make([]Account, 0, 101)
	for id := int64(1); id <= 101; id++ {
		accounts = append(accounts, quotaSnapshotParent(id, StatusActive, true))
	}
	repo := &quotaSnapshotRefreshRepoStub{accounts: accounts}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage()}
	lock := &quotaSnapshotLeaderLockStub{acquired: true, renewOK: false}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)
	svc.SetLeaderLock(lock, nil)

	err := svc.RunOnce(context.Background())
	require.Error(t, err, "a lost leader lock must fail the cycle")
	require.Len(t, quota.calls, 100, "only the first page may be processed before aborting")
	require.NotContains(t, quota.calls, int64(101), "the second page must not run after the lock is lost")
	require.Equal(t, 1, lock.renews)
	require.Equal(t, 1, lock.releases, "the lock must still be released via the deferred release")
}
