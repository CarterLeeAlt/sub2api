package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CUSTOM-011：新建 OpenAI OAuth/Setup Token 账号默认开启"仅允许 Codex 官方客户端"，
// 默认注入在后端创建逻辑完成（脚本建号、CRS 批量导入等绕过管理 UI 的路径同样生效）。
func TestEnsureCodexCLIOnlyDefaultForCreate(t *testing.T) {
	t.Run("OpenAI OAuth 未提供 → 注入 true", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeOAuth, nil)
		require.Equal(t, true, extra[codexCLIOnlyExtraKey])
	})

	t.Run("OpenAI OAuth extra 无键 → 注入 true", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{"other": 1})
		require.Equal(t, true, extra[codexCLIOnlyExtraKey])
		require.Equal(t, 1, extra["other"])
	})

	t.Run("OpenAI OAuth 显式 false → 尊重不覆盖", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{codexCLIOnlyExtraKey: false})
		require.Equal(t, false, extra[codexCLIOnlyExtraKey])
	})

	t.Run("OpenAI OAuth 显式 true → 保持", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{codexCLIOnlyExtraKey: true})
		require.Equal(t, true, extra[codexCLIOnlyExtraKey])
	})

	t.Run("OpenAI Setup Token 未提供 → 注入 true", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeSetupToken, nil)
		require.Equal(t, true, extra[codexCLIOnlyExtraKey])
	})

	t.Run("OpenAI API Key 不注入", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformOpenAI, AccountTypeAPIKey, nil)
		require.Nil(t, extra)
	})

	t.Run("非 OpenAI 平台不注入", func(t *testing.T) {
		extra := ensureCodexCLIOnlyDefaultForCreate(PlatformAnthropic, AccountTypeOAuth, nil)
		require.Nil(t, extra)
	})
}

// IsCodexCLIOnlyEnabled 的类型口径：OAuth 与 Setup Token 均生效，API Key 永不生效。
func TestAccount_IsCodexCLIOnlyEnabled_TypeCoverage(t *testing.T) {
	t.Run("OAuth true", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.True(t, account.IsCodexCLIOnlyEnabled())
	})

	t.Run("Setup Token true（此前读取口径不含 Setup Token）", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.True(t, account.IsCodexCLIOnlyEnabled())
	})

	t.Run("API Key 即使带键也 false", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.False(t, account.IsCodexCLIOnlyEnabled())
	})

	t.Run("字符串 true 不生效（严格 bool）", func(t *testing.T) {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexCLIOnlyExtraKey: "true"}}
		require.False(t, account.IsCodexCLIOnlyEnabled())
	})
}
