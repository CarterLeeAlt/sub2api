package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestUpdateCacheSeparatesUpstreamFromFork(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	forkKey := "update:latest:CarterLeeAlt/sub2api"
	require.NoError(t, rdb.Set(ctx, forkKey, `{"warning_code":"no_release"}`, time.Hour).Err())
	cache := NewUpdateCache(rdb)
	_, err := cache.GetUpdateInfo(ctx)
	require.ErrorIs(t, err, redis.Nil)
	require.NoError(t, cache.SetUpdateInfo(ctx, `{"latest":"0.2.22"}`, 1200*time.Second))
	data, err := cache.GetUpdateInfo(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{"latest":"0.2.22"}`, data)
	upstream, err := rdb.Get(ctx, "update:latest:Wei-Shaw/sub2api").Result()
	require.NoError(t, err)
	require.Equal(t, data, upstream)
	fork, err := rdb.Get(ctx, forkKey).Result()
	require.NoError(t, err)
	require.JSONEq(t, `{"warning_code":"no_release"}`, fork)
	server.FastForward(20 * time.Minute)
	_, err = cache.GetUpdateInfo(ctx)
	require.ErrorIs(t, err, redis.Nil)
}
