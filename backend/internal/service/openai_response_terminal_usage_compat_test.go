//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestEffectiveOpenAISSEEventTypePrefersPayload(t *testing.T) {
	t.Parallel()

	require.Equal(t, "response.failed", effectiveOpenAISSEEventType([]byte(`{"type":"response.failed"}`), "error"))
	require.Equal(t, "error", effectiveOpenAISSEEventType([]byte(`{"error":{"message":"failed"}}`), " error "))
	require.JSONEq(t, `{"type":"error","error":{"message":"failed"}}`, openAICompatPayloadWithEventType(`{"type":"","error":{"message":"failed"}}`, "error"))
}

func TestExtractOpenAISSETerminalEventUsesEventField(t *testing.T) {
	t.Parallel()

	body := "event: error\n" +
		"data: {\"error\":{\"message\":\"provider failed\"}}\n\n" +
		"data: [DONE]\n\n"
	eventType, payload, ok := extractOpenAISSETerminalEvent(body)
	require.True(t, ok)
	require.Equal(t, "error", eventType)
	require.Equal(t, "provider failed", extractOpenAISSEErrorMessage(payload))
}

func TestExtractOpenAISSETerminalEventUsesFinalAuthoritativeTerminal(t *testing.T) {
	t.Parallel()

	body := "data: {\"type\":\"error\",\"error\":{\"message\":\"recovering\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"
	eventType, payload, ok := extractOpenAISSETerminalEvent(body)
	require.True(t, ok)
	require.Equal(t, "response.completed", eventType)
	require.Equal(t, "resp_1", gjson.GetBytes(payload, "response.id").String())
}

func TestParseSSEUsageEffectiveTerminalRules(t *testing.T) {
	t.Parallel()

	svc := &OpenAIGatewayService{}
	usage := &OpenAIUsage{}
	svc.parseSSEUsageBytesWithType([]byte(`{"usage":{"input_tokens":17,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}}`), "response.in_progress", usage)
	svc.parseSSEUsageBytesWithType([]byte(`{"response":{"id":"resp_1"}}`), "response.completed", usage)
	require.Equal(t, OpenAIUsage{InputTokens: 17, OutputTokens: 5, CacheReadInputTokens: 3}, *usage)

	svc.parseSSEUsageBytesWithType([]byte(`{"response":{"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}}`), "response.completed", usage)
	require.Equal(t, OpenAIUsage{InputTokens: 17, OutputTokens: 5, CacheReadInputTokens: 3}, *usage)

	svc.parseSSEUsageBytesWithType([]byte(`{"response":{"usage":{"input_tokens":2,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}}`), "response.completed", usage)
	require.Equal(t, OpenAIUsage{InputTokens: 2}, *usage)
}

func TestOpenAIUsageSelectionUsesCorrespondingTerminalDetails(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, outer := range []string{`{}`, `null`, `{"input_tokens":0,"output_tokens":0}`} {
		t.Run(outer, func(t *testing.T) {
			usage := OpenAIUsage{InputTokens: 50, OutputTokens: 9, CacheReadInputTokens: 6, ImageInputTokens: 3}
			payload := `{"usage":` + outer + `,"data":{"response":{"usage":{"input_tokens":40,"output_tokens":4,"input_tokens_details":{"cached_tokens":0}}}}}`
			require.Equal(t, openAIUsageWithTokens, svc.parseSSEUsageBytesWithType([]byte(payload), "response.completed", &usage))
			require.Equal(t, OpenAIUsage{InputTokens: 40, OutputTokens: 4, ImageInputTokens: 3}, usage)
		})
	}
	usage, ok := extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"input_tokens":10},"response":{"usage":{"input_tokens":11,"output_tokens":5}}}`))
	require.True(t, ok)
	require.Equal(t, OpenAIUsage{InputTokens: 10}, usage, "the first usable report retains precedence")
}

func TestOpenAISSEUsageReportKeepsEvidenceAcrossFrames(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct {
		name, earlier, want string
	}{
		{name: "absent", want: "usage_field_absent"},
		{name: "null", earlier: `data: {"type":"response.in_progress","usage":null}` + "\n\n", want: "unusable_usage_field"},
		{name: "empty", earlier: `data: {"type":"response.in_progress","usage":{}}` + "\n\n", want: "no_positive_tokens"},
		{name: "zero", earlier: `data: {"type":"response.in_progress","usage":{"input_tokens":0,"output_tokens":0}}` + "\n\n", want: "no_positive_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, report := svc.parseSSEUsageFromBody(tc.earlier + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n")
			require.Equal(t, OpenAIUsage{}, *usage)
			require.Equal(t, tc.want, report.String())
		})
	}
}

func TestOpenAIResponsesMissingUsageWarningIncludesRequestAndReport(t *testing.T) {
	const events = `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" +
		`data: {"type":"response.in_progress","response":{"usage":null}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_test","status":"completed"}}` + "\n\n"
	for _, passthrough := range []bool{false, true} {
		openAIMissingUsageSampler.lastLog.Store(0)
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"upstream-test"}},
			Body:       io.NopCloser(strings.NewReader(events)),
		}}
		service := newOpenAIImageGenerationControlTestService(upstream)
		c, recorder := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.144.1")
		core, observed := observer.New(zap.WarnLevel)
		ctx := logger.IntoContext(c.Request.Context(), zap.New(core).With(zap.String("request_id", "gateway-test")))
		c.Request = c.Request.WithContext(ctx)
		account := newOpenAIImageGenerationControlTestAccount()
		account.Type = AccountTypeOAuth
		account.Credentials = map[string]any{"access_token": "fixture-token"}
		account.Extra = map[string]any{"openai_passthrough": passthrough}
		result, err := service.Forward(ctx, c, account, []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"continue"}`))
		require.NoError(t, err)
		require.Equal(t, OpenAIUsage{}, result.Usage)
		require.Contains(t, recorder.Body.String(), "hello")
		entries := observed.FilterMessage("openai_usage.success_missing_usage").All()
		require.Len(t, entries, 1)
		fields := entries[0].ContextMap()
		require.Equal(t, "gateway-test", fields["request_id"])
		require.Equal(t, "upstream-test", fields["upstream_request_id"])
		require.Equal(t, "unusable_usage_field", fields["usage_report"])
		require.Equal(t, "response.completed", fields["terminal_event"])
	}
}

func BenchmarkParseSSEUsageNoUsageDelta(b *testing.B) {
	svc := &OpenAIGatewayService{}
	usage := &OpenAIUsage{}
	payload := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.parseSSEUsageBytesWithType(payload, "response.output_text.delta", usage)
	}
}

func TestOpenAICompatTerminalResponseSynthesizesBareError(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"type":"error","code":"upstream_error","error":{"message":"provider failed"}}`)
	event := &apicompat.ResponsesStreamEvent{Type: "error", Code: "upstream_error"}
	response := openAICompatTerminalResponse(event, payload)
	require.NotNil(t, response)
	require.Equal(t, "failed", response.Status)
	require.Equal(t, "upstream_error", response.Error.Code)
	require.Equal(t, "provider failed", response.Error.Message)
}

func TestForEachOpenAISSEFrameDataTypeOverridesEventField(t *testing.T) {
	t.Parallel()

	var types []string
	forEachOpenAISSEFrame(strings.Join([]string{
		"event: response.in_progress",
		`data: {"type":"response.completed","response":{"id":"resp_1"}}`,
		"",
	}, "\n"), func(eventType string, _ []byte) {
		types = append(types, eventType)
	})
	require.Equal(t, []string{"response.completed"}, types)
}

func TestSampleOpenAIMissingUsageLogRateLimitsAndCounts(t *testing.T) {
	var sampler openAIMissingUsageLogSampler
	now := time.Unix(100, 0)
	logNow, total, suppressed := sampler.sample(now)
	require.True(t, logNow)
	require.Equal(t, uint64(1), total)
	require.Zero(t, suppressed)

	logNow, total, _ = sampler.sample(now.Add(time.Second))
	require.False(t, logNow)
	require.Equal(t, uint64(2), total)

	logNow, total, suppressed = sampler.sample(now.Add(openAIMissingUsageLogInterval))
	require.True(t, logNow)
	require.Equal(t, uint64(3), total)
	require.Equal(t, uint64(1), suppressed)
}
