package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// forwardOpenAIImagesOAuth (non-direct branch) must turn a failed dynamic
// main-model resolution into an account-level failover signal. A plain error
// would bypass both OpenAIImagesUpstreamError and UpstreamFailoverError
// handling in the handler and surface a hard 502 without switching accounts.
func TestForwardOpenAIImagesOAuthNoImageMainModelReturnsFailoverError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Manifest parses fine but exposes no API-supported image-capable model,
	// which makes resolveOpenAIImagesResponsesMainModel return an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5-text-only","supported_in_api":true,"priority":1,"input_modalities":["text"]}]}`))
	}))
	defer server.Close()

	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = originalURL }()

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:       771001,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-access-token",
			"chatgpt_account_id": "acc-771001",
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	// gpt-image-1 passes validateOpenAIImagesModel but is not a direct-images
	// model, so the request takes the Responses main-model branch.
	result, err := svc.forwardOpenAIImagesOAuth(context.Background(), c, account, &OpenAIImagesRequest{
		Endpoint: "/v1/images/generations",
		Model:    "gpt-image-1",
		Prompt:   "draw a cat",
		N:        1,
	}, "")
	require.Nil(t, result)
	require.Error(t, err)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "a missing image main model must fail over to another account")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.JSONEq(t, string(openAIImagesNoMainModelFailoverBody), string(failoverErr.ResponseBody))
	require.True(t, failoverErr.ShouldRetryNextAccount())
}

// The response.completed frame's tool_usage.image_gen block carries only the
// four primary token counters. Adopting it wholesale must not wipe cache read
// and cache creation details parsed off earlier SSE frames — the direct path
// (codexDirectImagesUsage) rebuilds those details, so both paths must agree.
func TestParseOpenAIImagesSSEUsageBytesPreservesCacheDetailsWithToolUsage(t *testing.T) {
	svc := &OpenAIGatewayService{}
	usage := OpenAIUsage{
		CacheReadInputTokens:     300,
		ImageCacheReadTokens:     120,
		CacheCreationInputTokens: 7,
	}
	data := []byte(`{"type":"response.completed","response":{"tool_usage":{"image_gen":{"input_tokens":50,"output_tokens":4096,"input_tokens_details":{"image_tokens":30},"output_tokens_details":{"image_tokens":4096}}}}}`)

	svc.parseOpenAIImagesSSEUsageBytes(data, &usage)

	// tool_usage counters win.
	require.Equal(t, 50, usage.InputTokens)
	require.Equal(t, 30, usage.ImageInputTokens)
	require.Equal(t, 4096, usage.OutputTokens)
	require.Equal(t, 4096, usage.ImageOutputTokens)
	// Cache details parsed earlier survive the completed frame.
	require.Equal(t, 300, usage.CacheReadInputTokens)
	require.Equal(t, 120, usage.ImageCacheReadTokens)
	require.Equal(t, 7, usage.CacheCreationInputTokens)
}

func TestParseOpenAIImagesSSEUsageBytesWithoutToolUsageKeepsParsedUsage(t *testing.T) {
	svc := &OpenAIGatewayService{}
	usage := OpenAIUsage{InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 33}
	// No response.usage and no tool_usage.image_gen: nothing may overwrite the
	// usage accumulated from earlier SSE frames.
	data := []byte(`{"type":"response.completed","response":{"tool_usage":{"other_tool":{"input_tokens":1}}}}`)

	svc.parseOpenAIImagesSSEUsageBytes(data, &usage)

	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 22, usage.OutputTokens)
	require.Equal(t, 33, usage.CacheReadInputTokens)
}

// Model text and diagnostic snippets can carry multi-byte UTF-8; truncation
// must stay on rune boundaries so client-facing errors and ops logs never
// contain invalid bytes.
func TestExtractOpenAIImagesModelTextTruncatesOnRuneBoundary(t *testing.T) {
	payload := strings.Repeat("a", 599) + "汉"
	body := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"` + payload + `"}]}]}}`)

	truncated := extractOpenAIImagesModelText(body)
	require.True(t, utf8.ValidString(truncated), "must not end with a partial rune")
	require.Len(t, []rune(truncated), 599, "the rune spanning the byte limit must be dropped whole")
}

func TestSummarizeOpenAIImagesNoOutputBodyTruncatesOnRuneBoundary(t *testing.T) {
	body := []byte(strings.Repeat("a", 1023) + "汉汉汉")

	summary := summarizeOpenAIImagesNoOutputBody(body)

	require.Contains(t, summary, "...(truncated)")
	require.True(t, utf8.ValidString(summary), "ops summary must not end with a partial rune")
}
