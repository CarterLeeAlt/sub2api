package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type AccountModelCatalogRepository interface {
	ListDueModelCatalogAccounts(context.Context, int64, int, bool) ([]Account, error)
	SaveModelCatalogObservation(context.Context, *Account, string, string, AccountModelCatalog) (bool, error)
	SetModelCatalogWake(func())
	FinishModelCatalogRound(context.Context, time.Time, time.Time) error
}

type upstreamModelCatalogReader interface {
	fetchOpenAICodexManifestEntries(context.Context, *Account, *Account) ([]openAICodexManifestModel, []byte, error)
}

// OpenAIModelCatalogRefreshService owns one serial worker. The database holds due
// times so restarts and notifications do not reset the upstream request cadence.
type OpenAIModelCatalogRefreshService struct {
	repo               AccountModelCatalogRepository
	reader             upstreamModelCatalogReader
	locks              LeaderLockCache
	owner              string
	ctx                context.Context
	cancel             context.CancelFunc
	wake               chan struct{}
	wg                 sync.WaitGroup
	settings           SettingRepository
	nextImageSpecFetch time.Time
}

func NewOpenAIModelCatalogRefreshService(repo AccountModelCatalogRepository, reader upstreamModelCatalogReader, locks LeaderLockCache) *OpenAIModelCatalogRefreshService {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIModelCatalogRefreshService{repo: repo, reader: reader, locks: locks, owner: uuid.NewString(), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
}

func (s *OpenAIModelCatalogRefreshService) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *OpenAIModelCatalogRefreshService) Start() {
	if s.settings != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			client := &http.Client{Timeout: 25 * time.Second}
			fetch := func(ctx context.Context, previous imageSpecSnapshot, now time.Time) (imageSpecSnapshot, error) {
				return fetchOfficialImageCatalog(ctx, client, officialImageSpecURL, officialModelDocsURL, previous, now)
			}
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				if err := s.refreshImageSpec(s.ctx, fetch); err != nil && s.ctx.Err() == nil {
					slog.Warn("openai_image_catalog_refresh_failed", "error", err)
				}
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			case <-s.wake:
			}
			if err := s.RunOnce(s.ctx); err != nil && s.ctx.Err() == nil {
				slog.Warn("model_catalog_refresh_failed", "error", err)
			}
		}
	}()
	s.Wake()
}
func (s *OpenAIModelCatalogRefreshService) Stop() { s.cancel(); s.wg.Wait() }

func (s *OpenAIModelCatalogRefreshService) RunOnce(ctx context.Context) error {
	const key = "openai:model-catalog:refresh:leader"
	const lease = 2 * time.Minute
	locked, err := s.locks.TryAcquireLeaderLock(ctx, key, s.owner, lease)
	if err != nil || !locked {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.locks.ReleaseLeaderLock(releaseCtx, key, s.owner); err != nil {
			slog.Warn("model_catalog_unlock_failed", "error", err)
		}
	}()
	var afterID int64
	startedAt := time.Now().UTC()
	for {
		queryCtx, queryCancel := context.WithTimeout(ctx, 10*time.Second)
		spec := officialImageModels.Load()
		accounts, err := s.repo.ListDueModelCatalogAccounts(queryCtx, afterID, 100, spec != nil && spec.Lifecycle != nil)
		queryCancel()
		if err != nil {
			return err
		}
		for i := range accounts {
			a := &accounts[i]
			afterID = a.ID
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ok, err := s.locks.RenewLeaderLock(ctx, key, s.owner, lease)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("model catalog leadership lost")
			}
			if !a.SupportsAutomaticModelCatalog() || a.ModelCatalog().Source != "upstream" {
				continue
			}
			if err := s.refresh(ctx, a); err != nil {
				slog.Warn("model_catalog_account_refresh_failed", "account_id", a.ID, "error", err)
			}
		}
		if len(accounts) < 100 {
			finishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			ok, err := s.locks.RenewLeaderLock(finishCtx, key, s.owner, lease)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("model catalog leadership lost")
			}
			return s.repo.FinishModelCatalogRound(finishCtx, startedAt, time.Now().UTC().Add(time.Duration(30+rand.IntN(16))*time.Minute))
		}
	}
}

func (s *OpenAIModelCatalogRefreshService) refresh(ctx context.Context, a *Account) error {
	state := a.ModelCatalog()
	expectedRevision := state.Revision
	credentials, err := json.Marshal(a.Credentials)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	spec := officialImageModels.Load()
	var entries []openAICodexManifestModel
	var fetchErr error
	if state.PendingImageManifest != "" && spec != nil && spec.Lifecycle != nil {
		fetchErr = json.Unmarshal([]byte(state.PendingImageManifest), &entries)
	} else {
		entries, _, fetchErr = s.reader.fetchOpenAICodexManifestEntries(requestCtx, a, a)
	}
	cancel()
	var models []string
	var pending bool
	if fetchErr == nil {
		models, pending, fetchErr = resolveOAuthCatalogModels(a, entries, spec)
	}
	now := time.Now().UTC()
	state.AttemptedAt = now.Format(time.RFC3339Nano)
	state.NextSyncAt = now.Add(time.Duration(30+rand.IntN(16)) * time.Minute).Format(time.RFC3339Nano)
	models = dedupeAndSortModelIDs(models)
	if fetchErr == nil && len(models) == 0 && !pending {
		fetchErr = fmt.Errorf("upstream returned an empty model list")
	}
	if fetchErr != nil {
		state.Error = SanitizeUpstreamErrorMessage(fetchErr.Error())
		// Temporary failures get a short follow-up; configuration and permanent
		// client errors retain the normal cadence until the account is corrected.
		var syncErr *UpstreamModelSyncError
		var manifestErr *codexModelsManifestUpstreamError
		var status int
		permanent := false
		if errors.As(fetchErr, &syncErr) {
			status = syncErr.StatusCode
			permanent = syncErr.Kind == UpstreamModelSyncErrorConfiguration || syncErr.Kind == UpstreamModelSyncErrorUnsupported
		}
		if errors.As(fetchErr, &manifestErr) {
			status = manifestErr.statusCode
		}
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			permanent = true
		}
		if !permanent {
			state.NextSyncAt = now.Add(2 * time.Minute).Format(time.RFC3339Nano)
		}
	} else {
		state.PendingImageManifest = ""
		if pending {
			saved, err := json.Marshal(entries)
			if err != nil {
				return err
			}
			state.PendingImageManifest = string(saved)
		}
		state.NativeImages = make(map[string]any)
		for _, model := range models {
			if IsGPTImageGenerationModel(model) {
				state.NativeImages[model] = true
			}
		}
		state.Added, state.Removed = ReplaceAccountModelWhitelist(a, models)
		state.SyncedAt = state.AttemptedAt
		state.Ready = len(models) != 0
		state.Error = ""
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer writeCancel()
	_, err = s.repo.SaveModelCatalogObservation(writeCtx, a, expectedRevision, string(credentials), state)
	if err != nil {
		return err
	}
	return fetchErr
}
