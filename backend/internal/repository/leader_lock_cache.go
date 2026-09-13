package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/redis/go-redis/v9"
)

const leaderLockKeyPrefix = "leader:lock:"

// leaderLockReleaseScript releases a leader lock only when the caller still owns
// it (compare-and-delete by owner token). This prevents a previous holder whose
// lock already expired — and was re-acquired by another instance — from deleting
// the new owner's lock.
var leaderLockReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

// leaderLockRenewScript extends a leader lock's TTL only when the caller still
// owns it (compare-and-expire by owner token). It is the renewal counterpart of
// leaderLockReleaseScript: a stale holder whose lease expired — and whose key
// was re-acquired by a peer — must never extend the peer's lock.
var leaderLockRenewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

type leaderLockCache struct {
	rdb *redis.Client
}

// NewLeaderLockCache returns a Redis-backed implementation of
// service.LeaderLockCache used by periodic background jobs to elect a single
// runner across instances.
func NewLeaderLockCache(rdb *redis.Client) service.LeaderLockCache {
	return &leaderLockCache{rdb: rdb}
}

func (c *leaderLockCache) TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, leaderLockKeyPrefix+key, owner, ttl).Result()
}

// RenewLeaderLock extends the lock TTL iff the key still holds the caller's
// owner token. It returns false (not an error) when ownership was lost.
func (c *leaderLockCache) RenewLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	result, err := leaderLockRenewScript.Run(ctx, c.rdb, []string{leaderLockKeyPrefix + key}, owner, ttl.Milliseconds()).Int()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			// The key is gone: the lock expired before renewal.
			return false, nil
		}
		return false, err
	}
	return result == 1, nil
}

func (c *leaderLockCache) ReleaseLeaderLock(ctx context.Context, key, owner string) error {
	return leaderLockReleaseScript.Run(ctx, c.rdb, []string{leaderLockKeyPrefix + key}, owner).Err()
}
