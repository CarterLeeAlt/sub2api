package service

import (
	"context"
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
}

func (l *quotaSnapshotLeaderLockStub) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	l.calls++
	return l.acquired, l.err
}

func (l *quotaSnapshotLeaderLockStub) ReleaseLeaderLock(context.Context, string, string) error {
	l.releases++
	return nil
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
	require.Equal(t, float64(75), repo.wham[shadow.ID]["codex_5h_used_percent"])
	require.Equal(t, 2, repo.reset[parent.ID].AvailableCount)
	require.Empty(t, repo.reset[parent.ID].Credits)
	require.Equal(t, repo.reset[parent.ID], repo.reset[shadow.ID])
	require.Equal(t, "2026-08-25T10:00:00.123456789Z", repo.reset[parent.ID].FetchedAt)
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

func TestOpenAIQuotaSnapshotRefreshConcurrencyIsBounded(t *testing.T) {
	accounts := make([]Account, 12)
	for i := range accounts {
		accounts[i] = quotaSnapshotParent(int64(i+1), StatusActive, true)
	}
	repo := &quotaSnapshotRefreshRepoStub{accounts: accounts}
	quota := &quotaSnapshotUsageReaderStub{usage: quotaSnapshotTestUsage(), delay: 25 * time.Millisecond}
	svc := NewOpenAIQuotaSnapshotRefreshService(repo, quota)

	require.NoError(t, svc.RunOnce(context.Background()))
	require.Equal(t, openAIQuotaSnapshotRefreshConcurrency, quota.maxActive)
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

	started := time.Now()
	require.NoError(t, svc.RunOnce(context.Background()))
	require.Less(t, time.Since(started), time.Second)
	require.Empty(t, repo.wham)
	require.Empty(t, repo.reset)
}
