package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// M4：compact 路径删除 client_metadata 后，prompt_cache_key 仍按账号 namespace scope；
// 普通路径的完整改写行为不受重构影响（由既有 account identity 测试覆盖）。
func TestScopeCodexAccountIdentityPromptCacheKeyInMap(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-e2e",
			"chatgpt_user_id":    "user-e2e",
		},
	}

	t.Run("有 namespace：scope 化 prompt_cache_key", func(t *testing.T) {
		body := map[string]any{"prompt_cache_key": "client-cache-key"}
		require.True(t, scopeCodexAccountIdentityPromptCacheKeyInMap(body, account, 7, ""))
		scoped, _ := body["prompt_cache_key"].(string)
		require.NotEqual(t, "client-cache-key", scoped)
		require.NotContains(t, scoped, "client-cache-key")
		// scope 输出确定：同一原始值、同一租户/账号重复 scope 得到同一派生值
		//（每次请求都从客户端原始值 scope 一次，不依赖幂等）。
		body2 := map[string]any{"prompt_cache_key": "client-cache-key"}
		require.True(t, scopeCodexAccountIdentityPromptCacheKeyInMap(body2, account, 7, ""))
		require.Equal(t, scoped, body2["prompt_cache_key"])
	})

	t.Run("无 namespace（非 OAuth 形态）：不改写", func(t *testing.T) {
		body := map[string]any{"prompt_cache_key": "client-cache-key"}
		require.False(t, scopeCodexAccountIdentityPromptCacheKeyInMap(body, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, 7, ""))
		require.Equal(t, "client-cache-key", body["prompt_cache_key"])
	})

	t.Run("空值不处理", func(t *testing.T) {
		require.False(t, scopeCodexAccountIdentityPromptCacheKeyInMap(map[string]any{}, account, 7, ""))
		require.False(t, scopeCodexAccountIdentityPromptCacheKeyInMap(nil, account, 7, ""))
	})
}

func TestApplyCodexAccountIdentityClientMetadataMap_ScopesPromptCacheKey(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-e2e",
			"chatgpt_user_id":    "user-e2e",
		},
	}
	body := map[string]any{
		"client_metadata":  map[string]any{"session_id": "sess-client"},
		"prompt_cache_key": "sess-client",
	}
	require.True(t, applyCodexAccountIdentityClientMetadataMap(body, account, 7))
	// prompt_cache_key 与 client_metadata.session_id 同值时按 session 口径 scope，
	// 两者派生自同一原始值。
	metadata, _ := body["client_metadata"].(map[string]any)
	scopedFromMetadata, _ := metadata["session_id"].(string)
	require.Equal(t, scopedFromMetadata, body["prompt_cache_key"])
}
