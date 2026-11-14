//go:build unit

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type gatedAuthRepository struct {
	service.APIKeyRepository
	read func(context.Context, string) (*service.APIKey, error)
}

func (r *gatedAuthRepository) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	return r.read(ctx, key)
}

type gatedAuthCache struct {
	service.APIKeyCache
	get      func(context.Context, string) (*service.APIKeyAuthCacheEntry, error)
	set      func(context.Context, string, *service.APIKeyAuthCacheEntry, time.Duration) error
	validate func(context.Context, string, string) (bool, error)
}

func (c *gatedAuthCache) GetAuthCache(ctx context.Context, key string) (*service.APIKeyAuthCacheEntry, error) {
	if c.get != nil {
		return c.get(ctx, key)
	}
	return c.APIKeyCache.GetAuthCache(ctx, key)
}

func (c *gatedAuthCache) SetAuthCache(ctx context.Context, key string, entry *service.APIKeyAuthCacheEntry, ttl time.Duration) error {
	if c.set != nil {
		return c.set(ctx, key, entry, ttl)
	}
	return c.APIKeyCache.SetAuthCache(ctx, key, entry, ttl)
}

func (c *gatedAuthCache) ValidateAuthCacheGeneration(ctx context.Context, key, generation string) (bool, error) {
	if c.validate != nil {
		return c.validate(ctx, key, generation)
	}
	return c.APIKeyCache.ValidateAuthCacheGeneration(ctx, key, generation)
}

func TestAuthCacheRedisRejectsRevokedInFlightResults(t *testing.T) {
	for _, stage := range []string{"database", "redis-read", "redis-write", "first-generation", "expired-generation"} {
		t.Run(stage, func(t *testing.T) {
			rdb1, rdb2 := resetCoordinationRedis(t), resetCoordinationRedis(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key := fmt.Sprintf("auth-race-%s-%d", stage, time.Now().UnixNano())
			digest := sha256.Sum256([]byte(key))
			hash := hex.EncodeToString(digest[:])
			cache1, cache2 := NewAPIKeyCache(rdb1), NewAPIKeyCache(rdb2)
			current := &crossInstanceAuthRepository{key: key}
			cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L1Size: 100, L1TTLSeconds: 60, L2TTLSeconds: 60, Singleflight: true}}
			a := service.NewAPIKeyService(current, nil, nil, nil, nil, cache1, cfg)
			if stage != "first-generation" {
				_, err := a.GetByKey(ctx, key)
				require.NoError(t, err)
				if stage != "redis-read" {
					require.NoError(t, rdb1.Del(ctx, apiKeyAuthCacheKey(hash)).Err())
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			gate := func() {
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			readerRepo := &gatedAuthRepository{APIKeyRepository: current, read: func(ctx context.Context, key string) (*service.APIKey, error) {
				value, err := current.GetByKeyForAuth(ctx, key)
				if stage == "database" || stage == "first-generation" || stage == "expired-generation" {
					gate()
				}
				return value, err
			}}
			readerCache := &gatedAuthCache{APIKeyCache: cache2}
			if stage == "redis-read" {
				readerCache.get = func(ctx context.Context, key string) (*service.APIKeyAuthCacheEntry, error) {
					entry, err := cache2.GetAuthCache(ctx, key)
					gate()
					return entry, err
				}
			}
			if stage == "redis-write" {
				readerCache.set = func(ctx context.Context, key string, entry *service.APIKeyAuthCacheEntry, ttl time.Duration) error {
					gate()
					return cache2.SetAuthCache(ctx, key, entry, ttl)
				}
			}
			b := service.NewAPIKeyService(readerRepo, nil, nil, nil, nil, readerCache, cfg)
			// No Pub/Sub subscriber: shared fencing must independently prevent a
			// stale fill even while the invalidation notification is delayed.
			type result struct {
				key *service.APIKey
				err error
			}
			done := make(chan result, 1)
			go func() { got, err := b.GetByKey(ctx, key); done <- result{got, err} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current.disabled.Store(true)
			a.InvalidateAuthCacheByKey(ctx, key)
			if stage == "expired-generation" {
				require.NoError(t, rdb1.PExpire(ctx, apiKeyAuthGenerationPrefix+hash, time.Millisecond).Err())
				require.Eventually(t, func() bool { return errors.Is(rdb1.Get(ctx, apiKeyAuthGenerationPrefix+hash).Err(), redis.Nil) }, time.Second, time.Millisecond)
			}
			unblock()
			select {
			case got := <-done:
				require.NoError(t, got.err)
				require.False(t, got.key.IsActive())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			local, err := b.GetByKey(ctx, key)
			require.NoError(t, err)
			require.False(t, local.IsActive())
			c := service.NewAPIKeyService(current, nil, nil, nil, nil, cache1, cfg)
			shared, err := c.GetByKey(ctx, key)
			require.NoError(t, err)
			require.False(t, shared.IsActive(), "a third instance must not see the revoked snapshot in Redis")
			entry, err := cache1.GetAuthCache(ctx, hash)
			require.NoError(t, err)
			require.NotEmpty(t, entry.Generation)
			require.Equal(t, service.StatusDisabled, entry.Snapshot.Status)
			require.Positive(t, rdb1.PTTL(ctx, apiKeyAuthGenerationPrefix+hash).Val())
		})
	}
}

func TestAuthCacheRedisFailureAndUnknownKeyBounds(t *testing.T) {
	rdb := resetCoordinationRedis(t)
	ctx := context.Background()
	base := NewAPIKeyCache(rdb)
	cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L2TTLSeconds: 60, NegativeTTLSeconds: 30}}
	unknown := fmt.Sprintf("unknown-%d", time.Now().UnixNano())
	missing := &gatedAuthRepository{read: func(context.Context, string) (*service.APIKey, error) { return nil, service.ErrAPIKeyNotFound }}
	svc := service.NewAPIKeyService(missing, nil, nil, nil, nil, base, cfg)
	_, err := svc.GetByKey(ctx, unknown)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	digest := sha256.Sum256([]byte(unknown))
	hash := hex.EncodeToString(digest[:])
	require.Zero(t, rdb.Exists(ctx, apiKeyAuthGenerationPrefix+hash, apiKeyAuthCacheKey(hash)).Val(), "random missing keys cannot allocate Redis data")

	key := fmt.Sprintf("failure-%d", time.Now().UnixNano())
	repo := &crossInstanceAuthRepository{key: key}
	warm := service.NewAPIKeyService(repo, nil, nil, nil, nil, base, cfg)
	_, err = warm.GetByKey(ctx, key)
	require.NoError(t, err)
	failure := errors.New("injected Redis generation read failure")
	fault := &gatedAuthCache{APIKeyCache: base, validate: func(context.Context, string, string) (bool, error) { return false, failure }}
	svc = service.NewAPIKeyService(repo, nil, nil, nil, nil, fault, cfg)
	got, err := svc.GetByKey(ctx, key)
	require.Nil(t, got)
	require.ErrorIs(t, err, service.ErrAPIKeyAuthUnavailable)
}
