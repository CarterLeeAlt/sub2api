package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	apiKeyRateLimitKeyPrefix   = "apikey:ratelimit:"
	apiKeyRateLimitDuration    = 24 * time.Hour
	apiKeyCreateCountKeyPrefix = "apikey:create_count:"
	apiKeyAuthCachePrefix      = "apikey:auth:"
	authCacheInvalidateChannel = "auth:cache:invalidate"
	apiKeyAuthGenerationPrefix = "apikey:auth:generation:"
	// Missing/expired generations must be initialized before a fresh DB read.
	// UUIDs prevent an expired or evicted generation from matching an old fill.
	apiKeyAuthGenerationTTL = 5 * time.Minute
)

// apiKeyRateLimitKey generates the Redis key for API key creation rate limiting.
func apiKeyRateLimitKey(userID int64) string {
	return fmt.Sprintf("%s%d", apiKeyRateLimitKeyPrefix, userID)
}

// apiKeyCreateCountKey generates the Redis key for per-user API key creation counting.
func apiKeyCreateCountKey(userID int64) string {
	return fmt.Sprintf("%s%d", apiKeyCreateCountKeyPrefix, userID)
}

func apiKeyAuthCacheKey(key string) string {
	return fmt.Sprintf("%s%s", apiKeyAuthCachePrefix, key)
}

type apiKeyCache struct {
	rdb *redis.Client
}

func NewAPIKeyCache(rdb *redis.Client) service.APIKeyCache {
	return &apiKeyCache{rdb: rdb}
}

func (c *apiKeyCache) GetCreateAttemptCount(ctx context.Context, userID int64) (int, error) {
	key := apiKeyRateLimitKey(userID)
	count, err := c.rdb.Get(ctx, key).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return count, err
}

func (c *apiKeyCache) IncrementCreateAttemptCount(ctx context.Context, userID int64) error {
	key := apiKeyRateLimitKey(userID)
	pipe := c.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, apiKeyRateLimitDuration)
	_, err := pipe.Exec(ctx)
	return err
}

// IncrementCreateCount 在固定窗口内累加创建次数并返回累加后的值。
// ExpireNX 只在首次创建计数键时设置过期，后续创建不会延长窗口；
// MULTI 保证 INCR 与 EXPIRE 同时生效，避免计数键丢失 TTL 后永久封禁。
func (c *apiKeyCache) IncrementCreateCount(ctx context.Context, userID int64, window time.Duration) (int64, error) {
	key := apiKeyCreateCountKey(userID)
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

func (c *apiKeyCache) IncrementDailyUsage(ctx context.Context, apiKey string) error {
	return c.rdb.Incr(ctx, apiKey).Err()
}

func (c *apiKeyCache) SetDailyUsageExpiry(ctx context.Context, apiKey string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, apiKey, ttl).Err()
}

func (c *apiKeyCache) GetAuthCache(ctx context.Context, key string) (*service.APIKeyAuthCacheEntry, error) {
	values, err := c.rdb.MGet(ctx, apiKeyAuthGenerationPrefix+key, apiKeyAuthCacheKey(key)).Result()
	if err != nil {
		return nil, err
	}
	generation, _ := values[0].(string)
	val, _ := values[1].(string)
	if generation == "" || val == "" {
		return &service.APIKeyAuthCacheEntry{Generation: generation}, nil
	}
	var entry service.APIKeyAuthCacheEntry
	if err := json.Unmarshal([]byte(val), &entry); err != nil {
		return nil, err
	}
	if entry.Generation != generation {
		return &service.APIKeyAuthCacheEntry{Generation: generation}, nil
	}
	return &entry, nil
}

var ensureAuthGenerationScript = redis.NewScript(`
local generation = redis.call('GET', KEYS[1])
if generation then return generation end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return ARGV[1]
`)

// Only called after the database confirms the key exists, so random invalid
// credentials cannot allocate Redis metadata.
func (c *apiKeyCache) EnsureAuthCacheGeneration(ctx context.Context, key string) (string, error) {
	return ensureAuthGenerationScript.Run(ctx, c.rdb, []string{apiKeyAuthGenerationPrefix + key}, uuid.NewString(), apiKeyAuthGenerationTTL.Milliseconds()).Text()
}

func (c *apiKeyCache) ValidateAuthCacheGeneration(ctx context.Context, key, generation string) (bool, error) {
	current, err := c.rdb.Get(ctx, apiKeyAuthGenerationPrefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return generation != "" && current == generation, err
}

var setAuthCacheScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
return 1
`)

func (c *apiKeyCache) SetAuthCache(ctx context.Context, key string, entry *service.APIKeyAuthCacheEntry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if entry.Generation == "" {
		return service.ErrAPIKeyAuthChanged
	}
	stored, err := setAuthCacheScript.Run(ctx, c.rdb, []string{apiKeyAuthGenerationPrefix + key, apiKeyAuthCacheKey(key)}, entry.Generation, payload, max(ttl.Milliseconds(), 1), max(ttl, apiKeyAuthGenerationTTL).Milliseconds()).Int()
	if err != nil {
		return err
	}
	if stored != 1 {
		return service.ErrAPIKeyAuthChanged
	}
	return nil
}

var deleteAuthCacheScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('DEL', KEYS[2])
return 1
`)

func (c *apiKeyCache) DeleteAuthCache(ctx context.Context, key string) error {
	return deleteAuthCacheScript.Run(ctx, c.rdb, []string{apiKeyAuthGenerationPrefix + key, apiKeyAuthCacheKey(key)}, uuid.NewString(), apiKeyAuthGenerationTTL.Milliseconds()).Err()
}

// PublishAuthCacheInvalidation publishes a cache invalidation message to all instances
func (c *apiKeyCache) PublishAuthCacheInvalidation(ctx context.Context, cacheKey string) error {
	return c.rdb.Publish(ctx, authCacheInvalidateChannel, cacheKey).Err()
}

// SubscribeAuthCacheInvalidation subscribes to cache invalidation messages
func (c *apiKeyCache) SubscribeAuthCacheInvalidation(ctx context.Context, handler func(cacheKey string)) error {
	pubsub := c.rdb.Subscribe(ctx, authCacheInvalidateChannel)

	// Verify subscription is working
	_, err := pubsub.Receive(ctx)
	if err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("subscribe to auth cache invalidation: %w", err)
	}

	defer func() {
		if err := pubsub.Close(); err != nil {
			log.Printf("Warning: failed to close auth cache invalidation pubsub: %v", err)
		}
	}()
	service.NotifyAuthCacheSubscriptionReady(ctx)

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return errors.New("auth cache invalidation pubsub channel closed")
			}
			if msg != nil {
				handler(msg.Payload)
			}
		}
	}
}
