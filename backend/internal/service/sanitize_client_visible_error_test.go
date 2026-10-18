package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 2026-10-18 客户端泄露面修复的回归测试：上游错误文案回传客户端前统一身份打码，
// Anthropic 透传/原生直通响应把上游模型名还原为客户端请求名。
func TestSanitizeUpstreamErrorBodyForClient(t *testing.T) {
	t.Run("Anthropic 形态：error.message 打码且结构保持", func(t *testing.T) {
		body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"workspace org_9xK2mNpQ8rSt (owner@example.com) rejected the request"}}`)
		out := sanitizeUpstreamErrorBodyForClient(body, "")
		require.Equal(t, "invalid_request_error", gjson.GetBytes(out, "error.type").String())
		require.Equal(t, "workspace *** (***) rejected the request", gjson.GetBytes(out, "error.message").String())
	})

	t.Run("OpenAI 形态：error.message 打码且其余字段保持", func(t *testing.T) {
		body := []byte(`{"error":{"message":"billing issue for acct_7Yh3Jk9LmN2pAbCd","type":"invalid_request_error","param":"model","code":"bad_account"}}`)
		out := sanitizeUpstreamErrorBodyForClient(body, "")
		require.Equal(t, "billing issue for ***", gjson.GetBytes(out, "error.message").String())
		require.Equal(t, "model", gjson.GetBytes(out, "error.param").String())
		require.Equal(t, "bad_account", gjson.GetBytes(out, "error.code").String())
	})

	t.Run("error 为字符串的退化形态同样打码", func(t *testing.T) {
		body := []byte(`{"error":"key sk-proj-AbCdEf1234567890GhIjKl is invalid"}`)
		out := sanitizeUpstreamErrorBodyForClient(body, "")
		require.Equal(t, "key sk-*** is invalid", gjson.GetBytes(out, "error").String())
	})

	t.Run("干净文案保持原样（结构不动）", func(t *testing.T) {
		body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`)
		require.Equal(t, body, sanitizeUpstreamErrorBodyForClient(body, ""))
	})

	t.Run("非 JSON 体回退为消毒摘要重建的 OpenAI 形状", func(t *testing.T) {
		out := sanitizeUpstreamErrorBodyForClient([]byte("<html>proxy error for owner@example.com</html>"), "proxy error for ***")
		require.Equal(t, "proxy error for ***", gjson.GetBytes(out, "error.message").String())
		require.Equal(t, "upstream_error", gjson.GetBytes(out, "error.type").String())
	})
	t.Run("非 JSON 体且摘要为空时使用通用文案", func(t *testing.T) {
		out := sanitizeUpstreamErrorBodyForClient([]byte("garbage"), "")
		require.NotEmpty(t, gjson.GetBytes(out, "error.message").String())
	})

	t.Run("结构化但无 error 文案的原样透传（detail 形态）", func(t *testing.T) {
		body := []byte(`{"detail":"Not Found"}`)
		require.Equal(t, body, sanitizeUpstreamErrorBodyForClient(body, "anything"))
	})

	t.Run("Anthropic 形态无 error.message 时同样原样透传", func(t *testing.T) {
		body := []byte(`{"type":"error","error":{"type":"permission_error"}}`)
		require.Equal(t, body, sanitizeUpstreamErrorBodyForClient(body, "summary"))
	})
}

func TestSanitizeAnthropicPassthroughSSELineForClient(t *testing.T) {
	t.Run("error 帧文案打码且保留 data 前缀", func(t *testing.T) {
		line := "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"account owner@example.com overloaded\"}}"
		out, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "", "")
		require.True(t, changed)
		require.Equal(t, "account *** overloaded", gjson.Get(out[len("data: "):], "error.message").String())
		require.True(t, gjson.Valid(out[len("data: "):]))
	})

	t.Run("message_start 的上游模型名回填为客户端请求名", func(t *testing.T) {
		line := `data: {"type":"message_start","message":{"model":"internal-glm-4.7","role":"assistant"}}`
		out, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "internal-glm-4.7", "glm-4.7")
		require.True(t, changed)
		require.Equal(t, "glm-4.7", gjson.Get(out[len("data: "):], "message.model").String())
	})

	t.Run("无映射（同名模型）不改写 message_start", func(t *testing.T) {
		line := `data: {"type":"message_start","message":{"model":"glm-4.7"}}`
		out, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "glm-4.7", "glm-4.7")
		require.False(t, changed)
		require.Equal(t, line, out)
	})

	t.Run("message_start 中的模型名与上游名不一致时不改写", func(t *testing.T) {
		line := `data: {"type":"message_start","message":{"model":"something-else"}}`
		_, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "internal-glm-4.7", "glm-4.7")
		require.False(t, changed)
	})

	t.Run("非 data 行与普通事件行原样返回", func(t *testing.T) {
		for _, line := range []string{
			"event: message_start",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"owner@example.com"}}`,
			"",
		} {
			out, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "a", "b")
			require.False(t, changed)
			require.Equal(t, line, out)
		}
	})

	t.Run("干净错误文案不触发改写", func(t *testing.T) {
		line := `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
		out, changed := sanitizeAnthropicPassthroughSSELineForClient(line, "", "")
		require.False(t, changed)
		require.Equal(t, line, out)
	})
}

func TestSanitizeOpenAIClientVisibleErrorPayload(t *testing.T) {
	t.Run("error 事件顶层 message 打码", func(t *testing.T) {
		payload := []byte(`{"type":"error","code":"server_error","message":"upstream acct_7Yh3Jk9LmN2pAbCd failed"}`)
		out, changed := sanitizeOpenAIClientVisibleErrorPayload(payload)
		require.True(t, changed)
		require.Equal(t, "upstream *** failed", gjson.GetBytes(out, "message").String())
		require.Equal(t, "server_error", gjson.GetBytes(out, "code").String())
	})

	t.Run("response.failed 的嵌套 error.message 打码", func(t *testing.T) {
		payload := []byte(`{"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_error","message":"contact owner@example.com"}}}`)
		out, changed := sanitizeOpenAIClientVisibleErrorPayload(payload)
		require.True(t, changed)
		require.Equal(t, "contact ***", gjson.GetBytes(out, "response.error.message").String())
		require.Equal(t, "resp_1", gjson.GetBytes(out, "response.id").String())
	})

	t.Run("非错误事件原样返回", func(t *testing.T) {
		payload := []byte(`{"type":"message_start","message":"owner@example.com"}`)
		out, changed := sanitizeOpenAIClientVisibleErrorPayload(payload)
		require.False(t, changed)
		require.Equal(t, payload, out)
	})

	t.Run("非法 JSON 原样返回", func(t *testing.T) {
		_, changed := sanitizeOpenAIClientVisibleErrorPayload([]byte("not json"))
		require.False(t, changed)
	})
}
