//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func resetCoordinationRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("SUB2API_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires isolated SUB2API_TEST_REDIS_ADDR")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(context.Background()).Err())
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

type resetCompletionFaultRepository struct {
	service.IdempotencyRepository
	db   *sql.DB
	fail atomic.Bool
}

func (r *resetCompletionFaultRepository) MarkSucceeded(ctx context.Context, id int64, status int, body string, expires time.Time) error {
	var scope string
	if err := r.db.QueryRowContext(ctx, `SELECT scope FROM idempotency_records WHERE id=$1`, id).Scan(&scope); err != nil {
		return err
	}
	if scope == "openai_reset_redeem" && r.fail.CompareAndSwap(true, false) {
		return errors.New("simulated completion write failure")
	}
	return r.IdempotencyRepository.MarkSucceeded(ctx, id, status, body, expires)
}

func TestResetCoordinationPostgresRedis(t *testing.T) {
	rdb1, rdb2 := resetCoordinationRedis(t), resetCoordinationRedis(t)
	accounts, db := oauthStateTestRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "057_add_idempotency_records.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	upstreamID := fmt.Sprintf("reset-%d", time.Now().UnixNano())
	makeAccount := func(name string) *service.Account {
		a := &service.Account{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "synthetic", "chatgpt_account_id": upstreamID, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
		require.NoError(t, accounts.Create(ctx, a))
		return a
	}
	a, duplicate := makeAccount("first"), makeAccount("duplicate")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var posts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
	}))
	defer upstream.Close()
	defer unblock()
	target, _ := url.Parse(upstream.URL)
	factory := func(string) (*req.Client, error) {
		return req.C().WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
			return func(r *req.Request) (*req.Response, error) {
				r.URL.Scheme = target.Scheme
				r.URL.Host = target.Host
				return rt.RoundTrip(r)
			}
		}), nil
	}
	base := NewIdempotencyRepository(nil, db)
	fault := &resetCompletionFaultRepository{IdempotencyRepository: base, db: db}
	fault.fail.Store(true)
	cfg := service.DefaultIdempotencyConfig()
	cfg.FailedRetryBackoff = 0
	first := service.NewOpenAIQuotaService(accounts, nil, service.NewOpenAITokenProvider(accounts, nil, nil), factory, nil)
	first.ConfigureRedemption(service.NewIdempotencyCoordinator(fault, cfg), NewLeaderLockCache(rdb1))
	second := service.NewOpenAIQuotaService(accounts, nil, service.NewOpenAITokenProvider(accounts, nil, nil), factory, nil)
	second.ConfigureRedemption(service.NewIdempotencyCoordinator(base, cfg), NewLeaderLockCache(rdb2))
	requestCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	done := make(chan error, 1)
	go func() { _, err := first.ResetCredit(requestCtx, a.ID, "same-confirmation"); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = second.ResetCredit(ctx, a.ID, "same-confirmation")
	require.ErrorIs(t, err, service.ErrIdempotencyInProgress)
	_, err = second.ResetCredit(ctx, duplicate.ID, "another-confirmation")
	require.Equal(t, "OPENAI_RESET_BUSY", infraerrors.Reason(err))
	disconnect()
	unblock()
	require.ErrorIs(t, <-done, service.ErrIdempotencyStoreUnavail)
	_, err = db.ExecContext(ctx, `UPDATE idempotency_records SET locked_until=NOW()-INTERVAL '1 minute' WHERE scope='openai_reset_redeem' AND status='processing'`)
	require.NoError(t, err)
	result, err := second.ResetCredit(ctx, a.ID, "same-confirmation")
	require.NoError(t, err)
	require.True(t, result.Replayed)
	require.Equal(t, 2, result.WindowsReset)
	require.Equal(t, int64(1), posts.Load())
	// An old owner's release/renew must not remove or extend a replacement lease.
	locks1, locks2 := NewLeaderLockCache(rdb1), NewLeaderLockCache(rdb2)
	key := "reset-recovery-test:" + upstreamID
	ok, err := locks1.TryAcquireLeaderLock(ctx, key, "old", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, rdb1.PExpire(ctx, leaderLockKeyPrefix+key, time.Millisecond).Err())
	require.Eventually(t, func() bool {
		ok, err := locks2.TryAcquireLeaderLock(ctx, key, "new", time.Minute)
		return err == nil && ok
	}, time.Second, 10*time.Millisecond)
	defer locks2.ReleaseLeaderLock(context.Background(), key, "new")
	require.NoError(t, locks1.ReleaseLeaderLock(ctx, key, "old"))
	ok, err = locks1.RenewLeaderLock(ctx, key, "old", time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "new", rdb2.Get(ctx, leaderLockKeyPrefix+key).Val())
}

type crossInstanceAuthRepository struct {
	service.APIKeyRepository
	key      string
	disabled atomic.Bool
}

func (r *crossInstanceAuthRepository) GetByKeyForAuth(context.Context, string) (*service.APIKey, error) {
	status := service.StatusActive
	if r.disabled.Load() {
		status = service.StatusDisabled
	}
	return &service.APIKey{ID: 1, UserID: 1, Key: r.key, Status: status, User: &service.User{ID: 1, Status: service.StatusActive}}, nil
}

func TestAuthRevocationRedisCrossInstance(t *testing.T) {
	rdb1, rdb2 := resetCoordinationRedis(t), resetCoordinationRedis(t)
	repo := &crossInstanceAuthRepository{key: fmt.Sprintf("sk-revoke-%d", time.Now().UnixNano())}
	cfg := &config.Config{}
	cfg.APIKeyAuth.L1Size = 100
	cfg.APIKeyAuth.L1TTLSeconds = 3600
	cfg.APIKeyAuth.L2TTLSeconds = 3600
	a := service.NewAPIKeyService(repo, nil, nil, nil, nil, NewAPIKeyCache(rdb1), cfg)
	b := service.NewAPIKeyService(repo, nil, nil, nil, nil, NewAPIKeyCache(rdb2), cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b.StartAuthCacheInvalidationSubscriber(ctx)
	defer b.StopAuthCacheInvalidationSubscriber()
	require.Eventually(t, func() bool { return b.AuthCacheInvalidationSubscriberHealth().Connected }, 2*time.Second, 10*time.Millisecond)
	for _, svc := range []*service.APIKeyService{a, b} {
		key, err := svc.GetByKey(ctx, repo.key)
		require.NoError(t, err)
		require.True(t, key.IsActive())
	}
	repo.disabled.Store(true)
	a.InvalidateAuthCacheByKey(ctx, repo.key)
	require.Eventually(t, func() bool { key, err := b.GetByKey(ctx, repo.key); return err == nil && !key.IsActive() }, 2*time.Second, 10*time.Millisecond)
	a.InvalidateAuthCacheByKey(ctx, repo.key)
}
