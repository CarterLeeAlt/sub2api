//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAINonStreamingReliability_AlwaysReturnsJSONForSSETerminals(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, terminal := range []string{"completed", "incomplete", "cancelled", "canceled"} {
			for _, contentType := range []string{"text/event-stream", ""} {
				t.Run(fmt.Sprintf("passthrough=%t/%s/content_type=%q", passthrough, terminal, contentType), func(t *testing.T) {
					c, rec := newNonStreamingFailoverContext(t)
					svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
					body := fmt.Sprintf("event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":{\"id\":\"resp_review\",\"object\":\"response\",\"created_at\":123,\"status\":%q,\"model\":\"gpt-upstream\",\"output\":[{\"id\":\"msg_review\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial answer\"}]}],\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\n", terminal, terminal, terminal)
					resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
					if contentType != "" {
						resp.Header.Set("Content-Type", contentType)
					}
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
					var usage *OpenAIUsage
					var responseID string
					if passthrough {
						result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "public-model", "gpt-upstream")
						require.NoError(t, err)
						require.NotNil(t, result)
						usage, responseID = result.usage, result.responseID
					} else {
						result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "public-model", "gpt-upstream")
						require.NoError(t, err)
						require.NotNil(t, result)
						usage, responseID = result.usage, result.responseID
					}
					require.True(t, json.Valid(rec.Body.Bytes()), "stream=false must not return raw SSE: %s", rec.Body.String())
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.Equal(t, "resp_review", responseID)
					require.Equal(t, terminal, gjson.GetBytes(rec.Body.Bytes(), "status").String())
					require.Equal(t, "public-model", gjson.GetBytes(rec.Body.Bytes(), "model").String())
					require.Equal(t, "partial answer", gjson.GetBytes(rec.Body.Bytes(), "output.0.content.0.text").String())
					require.Equal(t, 11, usage.InputTokens)
					require.Equal(t, 7, usage.OutputTokens)
				})
			}
		}
	}
}

func TestOpenAINonStreamingReliability_JSONTextContainingSSEFieldsRemainsJSON(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
			body := `{"id":"resp_json","object":"response","created_at":123,"status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"data: example\nevent: example"}]}],"usage":{"input_tokens":2,"output_tokens":3}}`
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			if passthrough {
				_, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-test", "gpt-test")
				require.NoError(t, err)
			} else {
				_, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-test", "gpt-test")
				require.NoError(t, err)
			}
			require.JSONEq(t, body, rec.Body.String())
		})
	}
}

func TestOpenAINonStreamingReliability_PassthroughPreservesCustomTools(t *testing.T) {
	for _, contentType := range []string{"", "text/event-stream"} {
		t.Run(fmt.Sprintf("content_type=%q", contentType), func(t *testing.T) {
			c, rec := newNonStreamingFailoverContext(t)
			body := openAIClientToolsRequest(false)
			sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tools\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"i1\",\"call_id\":\"c1\",\"name\":\"exec\",\"arguments\":\"{\\\"input\\\":\\\"pwd\\\"}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}
			if contentType != "" {
				resp.Header.Set("Content-Type", contentType)
			}
			upstream := &httpUpstreamRecorder{resp: resp}
			svc := openAIClientToolsTestService(upstream)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}
			_, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.4", false, nil, false, time.Now())
			require.NoError(t, err)
			assertOpenAIClientToolsLowered(t, upstream.lastBody)
			require.Equal(t, "custom_tool_call", gjson.GetBytes(rec.Body.Bytes(), "output.0.type").String())
			require.Equal(t, "pwd", gjson.GetBytes(rec.Body.Bytes(), "output.0.input").String())
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
		})
	}
}

func TestOpenAINonStreamingReliability_PreservesProgressiveUsage(t *testing.T) {
	for _, path := range []string{"responses", "passthrough", "compat_buffered", "compat_buffered_eof"} {
		for _, terminalUsage := range []string{
			`{"input_tokens":20,"output_tokens":5}`,
			`{"input_tokens":0,"output_tokens":0}`,
			`{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0,"image_tokens":0},"output_tokens_details":{"image_tokens":0}}`,
		} {
			t.Run(fmt.Sprintf("%s/terminal=%s", path, terminalUsage), func(t *testing.T) {
				c, _ := newNonStreamingFailoverContext(t)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				body := "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":8,\"cache_write_tokens\":3,\"image_tokens\":2},\"output_tokens_details\":{\"image_tokens\":4}}}}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_usage\",\"status\":\"completed\",\"output\":[],\"usage\":" + terminalUsage + "}}\n\n"
				if path == "compat_buffered_eof" {
					body = strings.TrimRight(body, "\n")
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				var usage *OpenAIUsage
				switch path {
				case "passthrough":
					result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-test", "gpt-test")
					require.NoError(t, err)
					usage = result.usage
				case "responses":
					result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-test", "gpt-test")
					require.NoError(t, err)
					usage = result.usage
				default:
					response, parsedUsage, _, _, err := svc.readOpenAICompatBufferedTerminal(resp, c, account, "test", "req_usage")
					require.NoError(t, err)
					require.NotNil(t, response)
					usage = &parsedUsage
				}
				require.Equal(t, 20, usage.InputTokens)
				require.Equal(t, 5, usage.OutputTokens)
				if strings.Contains(terminalUsage, "input_tokens_details") {
					require.Zero(t, usage.CacheReadInputTokens, "explicit terminal zeros remain authoritative")
					require.Zero(t, usage.CacheCreationInputTokens)
					require.Zero(t, usage.ImageInputTokens)
					require.Zero(t, usage.ImageOutputTokens)
				} else {
					require.Equal(t, 8, usage.CacheReadInputTokens)
					require.Equal(t, 3, usage.CacheCreationInputTokens)
					require.Equal(t, 2, usage.ImageInputTokens)
					require.Equal(t, 4, usage.ImageOutputTokens)
				}
			})
		}
	}
}
