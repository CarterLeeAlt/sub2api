//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// CC responses-shape 旁路（Cursor 类客户端把 Responses 形状 body 打到
// /v1/chat/completions）声明的 image_generation 工具产出图片时，必须按
// /v1/responses 相同口径把计费切到图片模型与尺寸档，而不是普通 token。
func TestChatBufferedStreamingResponse_ImageOutputsSwitchBillingModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sseBody := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_img\",\"model\":\"gpt-5.4\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_img\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[{\"type\":\"image_generation_call\",\"id\":\"ig_1\",\"result\":\"aGVsbG8=\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}}\n\n"

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"upstream-rid"}},
		Body:       io.NopCloser(strings.NewReader(sseBody)),
	}
	account := &Account{ID: 41, Name: "openai-oauth", Platform: PlatformOpenAI}
	counter := newOpenAIImageOutputCounter()
	cfg := OpenAIResponsesImageBillingConfig{Model: "gpt-image-2", SizeTier: "2k"}

	result, err := (&OpenAIGatewayService{}).handleChatBufferedStreamingResponse(
		resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now(), counter, cfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, "gpt-image-2", result.BillingModel)
	require.Equal(t, "2k", result.ImageSize)
}

// 上游不回显 size 时（桥接注入的工具不带 size 的典型产出），计数器必须解码
// b64 头部探测真实像素，避免计费档位全落到默认 2K（对 1K 产出多计费）。
func TestOpenAIImageOutputCounter_ProbesActualSizeWhenMissing(t *testing.T) {
	pngEncoded := encodeOpenAIImageTestPNG(t, 1024, 768)
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEData([]byte(fmt.Sprintf(
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"ig_probe","type":"image_generation_call","result":%q}]}}`,
		pngEncoded,
	)))

	require.Equal(t, 1, counter.Count())
	require.Equal(t, []string{"1024x768"}, counter.Sizes())
}

// 自报 size 仍然优先，不做重复探测。
func TestOpenAIImageOutputCounter_PrefersDeclaredSize(t *testing.T) {
	pngEncoded := encodeOpenAIImageTestPNG(t, 1024, 768)
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEData([]byte(fmt.Sprintf(
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"ig_declared","type":"image_generation_call","size":"2048x2048","result":%q}]}}`,
		pngEncoded,
	)))

	require.Equal(t, 1, counter.Count())
	require.Equal(t, []string{"2048x2048"}, counter.Sizes())
}

// 未接入图片计数器的路径（普通 CC / Grok 桥传 nil）不得改动计费字段。
func TestChatBufferedStreamingResponse_NilImageCounterKeepsTokenBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sseBody := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_txt\",\"model\":\"gpt-5.4\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_txt\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":6}}}\n\n"

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sseBody)),
	}
	account := &Account{ID: 42, Name: "openai-oauth", Platform: PlatformOpenAI}

	result, err := (&OpenAIGatewayService{}).handleChatBufferedStreamingResponse(
		resp, c, account, "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now(), nil, OpenAIResponsesImageBillingConfig{},
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Zero(t, result.ImageCount)
	require.Equal(t, "gpt-5.4", result.BillingModel)
}
