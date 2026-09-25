package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// M1：回传客户端的上游错误文本必须打码池化账号身份（email/org 标识/API key 片段）
// 与既有敏感 query 参数；打码只收紧、不改写其余内容。
func TestSanitizeUpstreamErrorMessage(t *testing.T) {
	t.Run("URL query 中的密钥参数打码（既有语义）", func(t *testing.T) {
		require.Equal(t,
			"request failed: https://x.example/v1?access_token=***&ok=1",
			SanitizeUpstreamErrorMessage("request failed: https://x.example/v1?access_token=abc123&ok=1"),
		)
	})

	t.Run("email 打码", func(t *testing.T) {
		require.Equal(t,
			"account *** exceeded quota",
			SanitizeUpstreamErrorMessage("account owner@example.com exceeded quota"),
		)
	})

	t.Run("org/acct 标识打码", func(t *testing.T) {
		require.Equal(t,
			"workspace *** is deactivated; *** OK",
			SanitizeUpstreamErrorMessage("workspace org_9xK2mNpQ8rSt is deactivated; acct_7Yh3Jk9LmN2p OK"),
		)
	})

	t.Run("API key 片段打码", func(t *testing.T) {
		require.Equal(t,
			"invalid key sk-*** provided",
			SanitizeUpstreamErrorMessage("invalid key sk-proj-AbCdEf1234567890GhIjKl provided"),
		)
	})

	t.Run("普通文本不受影响", func(t *testing.T) {
		msg := "The model `gpt-5.1-codex` does not exist or you do not have access to it."
		require.Equal(t, msg, SanitizeUpstreamErrorMessage(msg))
	})

	t.Run("session_ 一类近似词不误伤", func(t *testing.T) {
		msg := "session_id must be a uuid"
		require.Equal(t, msg, SanitizeUpstreamErrorMessage(msg))
	})

	t.Run("空串原样返回", func(t *testing.T) {
		require.Equal(t, "", SanitizeUpstreamErrorMessage(""))
	})
}
