//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func bufferedAnthropicReliabilityHandlers() map[string]compatStreamHandler {
	return map[string]compatStreamHandler{
		"chat_completions": func(svc *GatewayService, resp *http.Response, c *gin.Context) (*ForwardResult, error) {
			return svc.handleCCBufferedFromAnthropic(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now())
		},
		"responses": func(svc *GatewayService, resp *http.Response, c *gin.Context) (*ForwardResult, error) {
			return svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
		},
	}
}

func TestAnthropicBufferedReliability_RejectsFailedOrTruncatedStreams(t *testing.T) {
	upstreamError := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"temporarily overloaded\"}}\n\n"
	for name, handle := range bufferedAnthropicReliabilityHandlers() {
		for _, mode := range []string{"error_before_start", "error_after_start", "truncated", "read_error"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+name, nil)
				prefix := anthropicStreamTruncatedBeforeMessageDelta()
				var reader io.Reader = strings.NewReader(prefix)
				switch mode {
				case "error_before_start":
					reader = strings.NewReader(upstreamError)
				case "error_after_start":
					reader = strings.NewReader(prefix + upstreamError)
				case "read_error":
					reader = &errAfterReader{data: strings.NewReader(prefix), err: io.ErrUnexpectedEOF}
				}
				result, err := handle(&GatewayService{}, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(reader)}, c)
				require.Error(t, err, "an incomplete upstream must not become a successful JSON response")
				if strings.HasPrefix(mode, "error_") {
					var eventErr *sseStreamErrorEventError
					require.ErrorAs(t, err, &eventErr)
					require.Nil(t, result)
					require.False(t, c.Writer.Written(), "no output may be committed before failover")
				} else {
					require.Equal(t, http.StatusBadGateway, rec.Code)
					require.NotNil(t, result, "observed usage must remain available for billing")
					require.Equal(t, 20, result.Usage.InputTokens)
					require.Equal(t, 1, result.Usage.OutputTokens)
				}
			})
		}
	}
}

func TestAnthropicBufferedReliability_ConfiguredIdleTimeoutBoundsUpstreamRead(t *testing.T) {
	for name, handle := range bufferedAnthropicReliabilityHandlers() {
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+name, nil)
			resp, writer := stallingAnthropicUpstream(t, anthropicStreamTruncatedBeforeMessageDelta())
			done := make(chan compatStreamOutcome, 1)
			go func() {
				result, err := handle(newCompatStreamIdleTestService(1), resp, c)
				done <- compatStreamOutcome{result: result, err: err}
			}()
			select {
			case got := <-done:
				require.ErrorIs(t, got.err, errAnthropicNativeStreamIdle)
				require.Equal(t, http.StatusBadGateway, rec.Code)
			case <-time.After(3 * time.Second):
				_ = writer.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("buffered upstream reader did not stop after body closure")
				}
				t.Fatal("configured upstream idle timeout was not applied to the buffered request")
			}
		})
	}
}

func TestAnthropicBufferedReliability_AcceptsStopReasonWithoutMessageStop(t *testing.T) {
	for name, handle := range bufferedAnthropicReliabilityHandlers() {
		t.Run(name, func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(anthropicStreamWithoutMessageStop()))}
			result, err := handle(&GatewayService{}, resp, c)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 20, result.Usage.InputTokens)
		})
	}
}

func TestAnthropicBufferedReliability_ClientGoneDrainsUsageWithoutWriting(t *testing.T) {
	for name, handle := range bufferedAnthropicReliabilityHandlers() {
		t.Run(name, func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(anthropicStreamWithoutMessageStop()))}
			result, err := handle(&GatewayService{}, resp, c)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 20, result.Usage.InputTokens)
			require.Equal(t, 42, result.Usage.OutputTokens)
			require.Empty(t, rec.Body.String())
			require.False(t, c.Writer.Written())
		})
	}
}

func TestAnthropicBufferedReliability_RetryableErrorReachesFailover(t *testing.T) {
	for _, endpoint := range []string{"chat_completions", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`
			if endpoint == "responses" {
				body = `{"model":"claude-sonnet-4-6","input":"hi","stream":false}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
			upstreamBody := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"temporarily overloaded\"}}\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(upstreamBody))}
			svc := &GatewayService{
				cfg:              rawChatCompletionsTestConfig(),
				httpUpstream:     &anthropicHTTPUpstreamRecorder{resp: resp},
				rateLimitService: &RateLimitService{accountRepo: &errorPolicyRepoStub{}, cfg: &config.Config{}},
			}
			account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}}
			var err error
			if endpoint == "responses" {
				_, err = svc.ForwardAsResponses(context.Background(), c, account, []byte(body), nil)
			} else {
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(body), nil)
			}
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "upstream overload must remain eligible for account failover: %v", err)
			require.False(t, c.Writer.Written())
		})
	}
}
