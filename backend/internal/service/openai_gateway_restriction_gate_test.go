package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// EnforceCodexClientRestriction 是六个入口（/v1/responses、chat completions、WS、
// /v1/messages、alpha/search、live、images、embeddings）共用的 codex_cli_only 门，
// 这里用真实 detector 覆盖检测链路与拒绝响应语义；各入口的拒绝动作（403 JSON /
// Anthropic 错误 / WS 1008 关帧 / Live typed error）在各自 handler 组装。
func TestOpenAIGatewayService_EnforceCodexClientRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for k, v := range headers {
			c.Request.Header.Set(k, v)
		}
		return rec, c
	}

	codexUA := "codex_cli_rs/0.42.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
	officialHeaders := map[string]string{
		"User-Agent":        codexUA,
		"X-Codex-Window-Id": "test-window",
	}
	nonOfficialHeaders := map[string]string{"User-Agent": "curl/8.0"}

	t.Run("账号未开启开关：放行且不写响应", func(t *testing.T) {
		rec, c := newCtx(nonOfficialHeaders)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		require.False(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("开关开启 + 非官方客户端：拒绝并写 OpenAI 形状 403", func(t *testing.T) {
		rec, c := newCtx(nonOfficialHeaders)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.True(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Contains(t, rec.Body.String(), "This account only allows Codex official clients")
		require.Contains(t, rec.Body.String(), "forbidden_error")
	})

	t.Run("开关开启 + 官方客户端（UA + 指纹头）：放行", func(t *testing.T) {
		rec, c := newCtx(officialHeaders)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.False(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("Setup Token 账号同样受门管控", func(t *testing.T) {
		rec, c := newCtx(nonOfficialHeaders)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.True(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("API Key 账号不受此门管控", func(t *testing.T) {
		rec, c := newCtx(nonOfficialHeaders)
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.False(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("ForceCodexCLI 全局旁路放行", func(t *testing.T) {
		rec, c := newCtx(nonOfficialHeaders)
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: true}}}
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexCLIOnlyExtraKey: true}}
		require.False(t, svc.EnforceCodexClientRestriction(context.Background(), c, account, nil))
		require.Equal(t, http.StatusOK, rec.Code)
	})
}
