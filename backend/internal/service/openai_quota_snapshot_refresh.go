package service

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

const (
	openAIQuotaSnapshotRefreshPageSize       = 100
	openAIQuotaSnapshotRefreshConcurrency    = 4
	openAIQuotaSnapshotRefreshAccountTimeout = 20 * time.Second
	openAIQuotaSnapshotRefreshInitialMax     = 2 * time.Minute
	openAIQuotaSnapshotRefreshLeaderLockKey  = "openai:quota:snapshot:refresh:leader"
	openAIQuotaSnapshotRefreshLeaderLockTTL  = 30 * time.Minute
)

// OpenAIQuotaSnapshotRefreshRepository is intentionally narrower than
// AccountRepository so the periodic scanner can be tested without the rest of
// the account mutation surface.
type OpenAIQuotaSnapshotRefreshRepository interface {
	ListWithFilters(
		ctx context.Context,
		params pagination.PaginationParams,
		platform, accountType, status, search string,
		groupID int64,
		privacyMode string,
	) ([]Account, *pagination.PaginationResult, error)
	ListShadowsByParent(ctx context.Context, parentID int64) ([]*Account, error)
	UpdateOpenAICodexWhamSnapshotIfNewer(
		ctx context.Context,
		accountID int64,
		expectedWhamUpdatedAt string,
		updates map[string]any,
	) (bool, error)
	UpdateOpenAIResetCreditSnapshotIfNewer(
		ctx context.Context,
		accountID int64,
		expectedFetchedAt string,
		snapshot *OpenAIResetCreditSnapshot,
	) (bool, error)
}

type openAIQuotaUsageReader interface {
	QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
}

// OpenAIQuotaSnapshotRefreshService periodically refreshes read-only quota
// snapshots. It never calls ResetCredit or any other consumption API.
type OpenAIQuotaSnapshotRefreshService struct {
	repo         OpenAIQuotaSnapshotRefreshRepository
	quotaService openAIQuotaUsageReader
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	cycleMu      sync.Mutex
	started      bool
	stopped      bool

	now            func() time.Time
	initialDelay   func() time.Duration
	nextDelay      func() time.Duration
	accountTimeout time.Duration
}

func NewOpenAIQuotaSnapshotRefreshService(
	repo OpenAIQuotaSnapshotRefreshRepository,
	quotaService openAIQuotaUsageReader,
) *OpenAIQuotaSnapshotRefreshService {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIQuotaSnapshotRefreshService{
		repo:           repo,
		quotaService:   quotaService,
		instanceID:     uuid.NewString(),
		parentCtx:      ctx,
		parentCancel:   cancel,
		now:            time.Now,
		initialDelay:   randomOpenAIQuotaSnapshotInitialDelay,
		nextDelay:      randomOpenAIQuotaSnapshotRefreshDelay,
		accountTimeout: openAIQuotaSnapshotRefreshAccountTimeout,
	}
}

func (s *OpenAIQuotaSnapshotRefreshService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

func (s *OpenAIQuotaSnapshotRefreshService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *OpenAIQuotaSnapshotRefreshService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *OpenAIQuotaSnapshotRefreshService) runLoop() {
	defer s.wg.Done()
	delay := s.initialDelay()
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.parentCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}

		if err := s.RunOnce(s.parentCtx); err != nil {
			slog.Warn("openai_quota_snapshot_refresh_cycle_failed", "error", err)
		}
		delay = s.nextDelay()
	}
}

func randomOpenAIQuotaSnapshotInitialDelay() time.Duration {
	return time.Duration(rand.Int64N(int64(openAIQuotaSnapshotRefreshInitialMax) + 1))
}

func randomOpenAIQuotaSnapshotRefreshDelay() time.Duration {
	return openAIQuotaSnapshotRefreshDelay(rand.IntN(6))
}

func openAIQuotaSnapshotRefreshDelay(randomIndex int) time.Duration {
	return time.Duration(10+randomIndex) * time.Minute
}

// RunOnce scans every undeleted OpenAI OAuth parent account. Persistent status,
// schedulable, cooldown and transient rate-limit state are deliberately not
// considered: display caches must keep refreshing even when routing is off.
func (s *OpenAIQuotaSnapshotRefreshService) RunOnce(ctx context.Context) error {
	if s == nil || s.repo == nil || s.quotaService == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	release, acquired, err := s.tryAcquireLeaderLock(ctx)
	if err != nil {
		return fmt.Errorf("acquire OpenAI quota snapshot refresh leader lock: %w", err)
	}
	if !acquired {
		return nil
	}
	defer release()

	for page := 1; ; page++ {
		accounts, result, err := s.repo.ListWithFilters(
			ctx,
			pagination.PaginationParams{
				Page:      page,
				PageSize:  openAIQuotaSnapshotRefreshPageSize,
				SortBy:    "id",
				SortOrder: pagination.SortOrderAsc,
			},
			PlatformOpenAI,
			AccountTypeOAuth,
			"",
			"",
			0,
			"",
		)
		if err != nil {
			return fmt.Errorf("list OpenAI OAuth accounts page %d: %w", page, err)
		}

		group, groupCtx := errgroup.WithContext(ctx)
		group.SetLimit(openAIQuotaSnapshotRefreshConcurrency)
		for i := range accounts {
			account := accounts[i]
			if !account.IsOpenAIOAuth() || account.IsShadow() {
				continue
			}
			group.Go(func() error {
				accountCtx, cancel := context.WithTimeout(groupCtx, s.accountTimeout)
				defer cancel()
				if err := s.refreshAccount(accountCtx, &account); err != nil {
					slog.Warn("openai_quota_snapshot_refresh_account_failed", "account_id", account.ID, "error", err)
				}
				return nil
			})
		}
		_ = group.Wait()

		if len(accounts) < openAIQuotaSnapshotRefreshPageSize || result == nil || page >= result.Pages {
			return nil
		}
	}
}

func (s *OpenAIQuotaSnapshotRefreshService) refreshAccount(ctx context.Context, account *Account) error {
	usage, err := s.quotaService.QueryUsage(ctx, account.ID)
	if err != nil {
		return err
	}
	if usage == nil {
		return fmt.Errorf("upstream returned an empty quota snapshot")
	}
	observedAt := s.now().UTC()

	var firstErr error
	if err := s.persistWhamSnapshot(ctx, account.ID, buildCodexWhamWindowExtraUpdates(usage, observedAt, false)); err != nil {
		firstErr = err
	}
	if err := s.persistResetCreditSnapshot(ctx, account.ID, usage.RateLimitResetCredits, observedAt); err != nil && firstErr == nil {
		firstErr = err
	}

	shadows, err := s.repo.ListShadowsByParent(ctx, account.ID)
	if err != nil {
		if firstErr != nil {
			return fmt.Errorf("persist parent snapshot: %v; list Spark shadows: %w", firstErr, err)
		}
		return fmt.Errorf("list Spark shadows: %w", err)
	}
	for _, shadow := range shadows {
		if shadow == nil || !shadow.IsShadow() {
			continue
		}
		if err := s.persistWhamSnapshot(ctx, shadow.ID, buildCodexSparkWindowExtraUpdates(usage, observedAt)); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.persistResetCreditSnapshot(ctx, shadow.ID, usage.RateLimitResetCredits, observedAt); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *OpenAIQuotaSnapshotRefreshService) persistWhamSnapshot(ctx context.Context, accountID int64, updates map[string]any) error {
	generation := codexWhamSnapshotGeneration(updates)
	if generation == "" {
		return nil
	}
	_, err := s.repo.UpdateOpenAICodexWhamSnapshotIfNewer(ctx, accountID, generation, updates)
	return err
}

func (s *OpenAIQuotaSnapshotRefreshService) persistResetCreditSnapshot(
	ctx context.Context,
	accountID int64,
	credits *OpenAIRateLimitResetCredits,
	observedAt time.Time,
) error {
	if credits == nil {
		return nil
	}
	fetchedAt := formatCodexWhamSnapshotGeneration(observedAt)
	snapshot := &OpenAIResetCreditSnapshot{
		AvailableCount: credits.AvailableCount,
		Credits:        append([]OpenAIRateLimitResetCreditDetail(nil), credits.Credits...),
		FetchedAt:      fetchedAt,
	}
	_, err := s.repo.UpdateOpenAIResetCreditSnapshotIfNewer(ctx, accountID, fetchedAt, snapshot)
	return err
}

func (s *OpenAIQuotaSnapshotRefreshService) tryAcquireLeaderLock(ctx context.Context) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(
			lockCtx,
			openAIQuotaSnapshotRefreshLeaderLockKey,
			s.instanceID,
			openAIQuotaSnapshotRefreshLeaderLockTTL,
		)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.lockCache.ReleaseLeaderLock(releaseCtx, openAIQuotaSnapshotRefreshLeaderLockKey, s.instanceID)
		}, true, nil
	}
	if s.db != nil {
		return tryAcquireDBAdvisoryLockWithError(lockCtx, s.db, hashAdvisoryLockID(openAIQuotaSnapshotRefreshLeaderLockKey))
	}
	return func() {}, true, nil
}
