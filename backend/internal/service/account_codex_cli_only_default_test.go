package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// IsCodexCLIOnlyEnabled 的类型口径：OAuth 与 Setup Token 均生效，API Key 永不生效。
// 注：CUSTOM-011 的"新建账号默认注入 codex_cli_only=true"已于 2026-10-04 按用户决定
// 退役，新建账号默认关闭（extra 不落键）；以下读取口径语义保持不变，历史账号的
// 已存 true 值继续生效。
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
