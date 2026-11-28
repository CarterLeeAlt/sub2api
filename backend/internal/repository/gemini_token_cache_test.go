//go:build unit

package repository

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGeminiTokenCache_OpenAITokenReadWarnings(t *testing.T) {
	ctx := t.Context()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := NewGeminiTokenCache(rdb)
	provider := service.NewOpenAITokenProvider(nil, cache, nil)
	account := &service.Account{
		ID:       1,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "account-token",
			"expires_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	cacheKey := service.OpenAITokenCacheKey(account)
	const warning = "openai_token_cache_get_failed"
	var logOutput bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	for _, scenario := range []struct {
		name    string
		prepare func(t *testing.T)
	}{
		{name: "uncached"},
		{name: "expired", prepare: func(t *testing.T) {
			require.NoError(t, cache.SetAccessToken(ctx, cacheKey, "expired-token", time.Minute))
			server.FastForward(time.Minute)
		}},
		{name: "deleted", prepare: func(t *testing.T) {
			require.NoError(t, cache.DeleteAccessToken(ctx, cacheKey))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.prepare != nil {
				scenario.prepare(t)
			}
			logOutput.Reset()
			token, err := cache.GetAccessToken(ctx, cacheKey)
			require.NoError(t, err)
			require.Empty(t, token)

			token, err = provider.GetAccessToken(ctx, account)
			require.NoError(t, err)
			require.Equal(t, "account-token", token)
			require.NotContains(t, logOutput.String(), warning)
			cachedToken, err := cache.GetAccessToken(ctx, cacheKey)
			require.NoError(t, err)
			require.Equal(t, token, cachedToken)
		})
	}

	t.Run("cache hit", func(t *testing.T) {
		require.NoError(t, cache.SetAccessToken(ctx, cacheKey, "cached-token", time.Minute))
		logOutput.Reset()
		token, err := provider.GetAccessToken(ctx, account)
		require.NoError(t, err)
		require.Equal(t, "cached-token", token)
		require.NotContains(t, logOutput.String(), warning)
	})

	t.Run("Redis read error", func(t *testing.T) {
		require.NoError(t, cache.DeleteAccessToken(ctx, cacheKey))
		_, err := server.RPush(oauthTokenKeyPrefix+cacheKey, "wrong-value-type")
		require.NoError(t, err)
		_, err = cache.GetAccessToken(ctx, cacheKey)
		require.ErrorContains(t, err, "WRONGTYPE")

		logOutput.Reset()
		token, err := provider.GetAccessToken(ctx, account)
		require.NoError(t, err)
		require.Equal(t, "account-token", token)
		require.Contains(t, logOutput.String(), warning)
		require.Contains(t, logOutput.String(), "WRONGTYPE")
	})
}

func TestGeminiTokenCache_DeleteAccessToken_RedisError(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = rdb.Close()
	})

	cache := NewGeminiTokenCache(rdb)
	err := cache.DeleteAccessToken(context.Background(), "broken")
	require.Error(t, err)
}
