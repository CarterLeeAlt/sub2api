//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newLeaderLockTestCache(t *testing.T) (*leaderLockCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &leaderLockCache{rdb: rdb}, mr
}

func TestLeaderLockCache_AcquireContendedRelease(t *testing.T) {
	cache, _ := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "dashboard:aggregation:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "first owner should acquire")

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "peer must be locked out while held")

	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, "A"))

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "peer should acquire after release")
}

// A stale owner whose lock expired and was re-acquired by a peer must not delete
// the peer's lock when its late release fires (compare-and-delete by owner).
func TestLeaderLockCache_ReleaseIsCompareAndDelete(t *testing.T) {
	cache, _ := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "payment:order:expiry:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate A's lock expiring and peer B taking ownership.
	require.NoError(t, cache.rdb.Set(ctx, leaderLockKeyPrefix+key, "B", time.Minute).Err())

	// A's stale release must be a no-op against B's lock.
	require.NoError(t, cache.ReleaseLeaderLock(ctx, key, "A"))

	val, err := cache.rdb.Get(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Equal(t, "B", val, "stale owner must not delete the new owner's lock")
}

func TestLeaderLockCache_TTLExpires(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "subscription:expiry:reminder:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	mr.FastForward(2 * time.Minute)

	ok, err = cache.TryAcquireLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "lock should be re-acquirable after the TTL expires")
}

// Renewal is compare-and-expire: only the current owner can extend the TTL. A
// stale holder whose lease expired — and whose key was re-acquired by a peer —
// must never extend the new owner's lock.
func TestLeaderLockCache_RenewIsCompareAndExpire(t *testing.T) {
	cache, _ := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "openai:quota:snapshot:refresh:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = cache.RenewLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "the current owner should renew")

	ok, err = cache.RenewLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "a peer must not renew someone else's lock")
}

// Renewal must actually extend the lease: after the original TTL has passed the
// key survives because the owner renewed with a longer TTL.
func TestLeaderLockCache_RenewExtendsTTL(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "openai:quota:snapshot:refresh:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = cache.RenewLeaderLock(ctx, key, "A", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	mr.FastForward(2 * time.Minute)

	val, err := cache.rdb.Get(ctx, leaderLockKeyPrefix+key).Result()
	require.NoError(t, err)
	require.Equal(t, "A", val, "renewed lock must outlive the original TTL")

	ok, err = cache.RenewLeaderLock(ctx, key, "B", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "a peer must not renew someone else's lock")
}

// Once the key is gone (expired or deleted) renewal must report a lost lock
// instead of an error.
func TestLeaderLockCache_RenewAfterExpiryReturnsFalse(t *testing.T) {
	cache, mr := newLeaderLockTestCache(t)
	ctx := context.Background()
	const key = "openai:quota:snapshot:refresh:leader"

	ok, err := cache.TryAcquireLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	mr.FastForward(2 * time.Minute)

	ok, err = cache.RenewLeaderLock(ctx, key, "A", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "renewing an expired lock reports a lost lock, not an error")
}
