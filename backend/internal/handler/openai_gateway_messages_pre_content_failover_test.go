//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	preContentHandlerCreated = `data: {"type":"response.created","response":{"id":"resp_pc","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}` + "\n\n"
	preContentHandlerFailed  = `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"An error occurred while processing your request."}}}` + "\n\n"
	preContentHandlerSuccess = preContentHandlerCreated +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"answer from second account"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_pc","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}` + "\n\n"
)

// preContentFailoverUpstream hands each upstream request to respond, which
// gets the 1-based call number.
type preContentFailoverUpstream struct {
	service.HTTPUpstream
	mu      sync.Mutex
	hits    []int64
	bodies  []string
	respond func(call int) (*http.Response, error)
}

func (u *preContentFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.mu.Lock()
	u.hits = append(u.hits, accountID)
	u.bodies = append(u.bodies, string(body))
	call := len(u.hits)
	u.mu.Unlock()
	return u.respond(call)
}

func (u *preContentFailoverUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.hits)
}

func preContentSSEResponse(body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}
}

// preContentStallThenFail stalls past one keepalive tick after the first event
// and then fails with failed.
func preContentStallThenFail(failed string) *http.Response {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, preContentHandlerCreated)
		time.Sleep(1300 * time.Millisecond)
		_, _ = io.WriteString(pw, failed)
		_ = pw.Close()
	}()
	return preContentSSEResponse(pr)
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type preContentFailoverScenario struct {
	ownedResponse string
	endpoint      string
	usageLogs     service.UsageLogRepository
	billing       *compatStreamUsageBillingRepo
	accounts      []service.Account
	respond       func(call int) (*http.Response, error)
	passthrough   *service.ErrorPassthroughService
	requestBody   string
}

func preContentAccounts(count int) []service.Account {
	accounts := make([]service.Account, 0, count)
	for i := 0; i < count; i++ {
		accounts = append(accounts, service.Account{
			ID: int64(9201 + i), Name: "openai-apikey", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: i + 1,
			Credentials: map[string]any{"api_key": "sk-test", "base_url": "http://upstream.example"},
		})
	}
	return accounts
}

func servePreContentFailoverMessages(t *testing.T, accountCount int) (*httptest.ResponseRecorder, *preContentFailoverUpstream) {
	t.Helper()
	return serveMessagesScenario(t, preContentFailoverScenario{
		accounts: preContentAccounts(accountCount),
		respond: func(call int) (*http.Response, error) {
			if call == 1 {
				return preContentStallThenFail(preContentHandlerFailed), nil
			}
			return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentHandlerSuccess))), nil
		},
	})
}

func serveMessagesScenario(t *testing.T, sc preContentFailoverScenario) (*httptest.ResponseRecorder, *preContentFailoverUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(9101)
	repo := &grokCredentialHandlerRepo{accounts: sc.accounts, missingOnGet: map[int64]bool{}}
	upstream := &preContentFailoverUpstream{respond: sc.respond}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	var billingRepo service.UsageBillingRepository
	if sc.billing != nil {
		billingRepo = sc.billing
		cfg.RunMode = config.RunModeStandard
	}
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Gateway.StreamKeepaliveInterval = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cacheBalance := &wsTurnBillingCache{}
	cacheBalance.balance.Store(100)
	billingCache := service.NewBillingCacheService(cacheBalance, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(
		repo, sc.usageLogs, billingRepo, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billingCache, &service.APIKeyService{}, nil, sc.passthrough, nil, nil, cfg)
	apiKey := &service.APIKey{
		ID: 9102, UserID: 9103, GroupID: &groupID,
		User:  &service.User{ID: 9103, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true, RateMultiplier: 1},
	}
	if sc.ownedResponse != "" {
		require.NoError(t, gateway.BindOpenAIHTTPResponseOwner(t.Context(), groupID, sc.ownedResponse, apiKey.User.ID, apiKey.ID))
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		service.BindErrorPassthroughService(c, sc.passthrough)
		c.Next()
	})
	router.POST("/v1/messages", h.Messages)
	router.POST("/v1/responses", h.Responses)
	router.POST("/v1/chat/completions", h.ChatCompletions)

	rec := httptest.NewRecorder()
	requestBody := sc.requestBody
	if requestBody == "" {
		requestBody = `{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	}
	endpoint := sc.endpoint
	if endpoint == "" {
		endpoint = "/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(requestBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec, upstream
}

func TestOpenAIInterruptedResponsesBilling(t *testing.T) {
	for _, mode := range []string{"native", "passthrough"} {
		for _, report := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reported=%t", mode, report), func(t *testing.T) {
				accounts := preContentAccounts(2)
				if mode == "passthrough" {
					for i := range accounts {
						accounts[i].Extra = map[string]any{"openai_passthrough": true}
					}
				}
				logs := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 3)}
				billing := &compatStreamUsageBillingRepo{}
				usage := ""
				if report {
					usage = `,"usage":{"input_tokens":7,"output_tokens":2}`
				}
				stream := preContentHandlerCreated + `data: {"type":"response.output_text.delta","delta":"partial answer"}` + "\n\n" + `data: {"type":"response.failed","response":{"id":"resp_pc","status":"failed","error":{"code":"server_error","message":"interrupted"}` + usage + `}}` + "\n\n"
				rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{accounts: accounts, endpoint: "/v1/responses", requestBody: `{"model":"gpt-5.4","input":"hello","stream":true}`, usageLogs: logs, billing: billing, respond: func(int) (*http.Response, error) {
					return preContentSSEResponse(io.NopCloser(strings.NewReader(stream))), nil
				}})
				require.Contains(t, rec.Body.String(), "partial answer")
				require.Contains(t, rec.Body.String(), "response.failed")
				require.NotContains(t, rec.Body.String(), "response.completed")
				require.Equal(t, 1, upstream.calls())
				if report {
					require.Len(t, logs.created, 1)
					row := <-logs.created
					require.Equal(t, 7, row.InputTokens)
					require.Equal(t, 2, row.OutputTokens)
					require.Len(t, billing.commands, 1)
					require.Positive(t, row.ActualCost)
				} else {
					require.Empty(t, billing.commands)
					require.Empty(t, logs.created)
				}
			})
		}
	}
}

func TestOpenAICompletedResponsesBilling(t *testing.T) {
	const output = `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"completed answer"}` + "\n\n"
	const usage = `{"input_tokens":10,"output_tokens":5}`
	const completed = `"id":"resp_pc","object":"response","status":"completed","output":[]`
	for _, tc := range []struct {
		name      string
		earlier   string
		terminal  string
		wantUsage bool
	}{
		{name: "reported", terminal: `"response":{` + completed + `,"usage":` + usage + `}`, wantUsage: true},
		{name: "reported earlier", earlier: `data: {"type":"response.in_progress","response":{"usage":` + usage + `}}` + "\n\n", terminal: `"response":{` + completed + `}`, wantUsage: true},
		{name: "empty outer usage", terminal: `"usage":{},"response":{` + completed + `,"usage":` + usage + `}`, wantUsage: true},
		{name: "absent", terminal: `"response":{` + completed + `}`},
	} {
		for _, passthrough := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/stream=%t", tc.name, passthrough, stream), func(t *testing.T) {
					accounts := preContentAccounts(2)
					for i := range accounts {
						accounts[i].Type = service.AccountTypeOAuth
						accounts[i].Credentials = map[string]any{"access_token": "fixture-token"}
						accounts[i].Extra = map[string]any{"openai_passthrough": passthrough}
					}
					logs := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
					billing := &compatStreamUsageBillingRepo{}
					events := preContentHandlerCreated + tc.earlier + output + `data: {"type":"response.completed",` + tc.terminal + `}` + "\n\n"
					rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
						accounts: accounts, endpoint: "/v1/responses", usageLogs: logs, billing: billing,
						requestBody: fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","stream":%t}`, stream),
						respond: func(int) (*http.Response, error) {
							return preContentSSEResponse(io.NopCloser(strings.NewReader(events))), nil
						},
					})
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "completed answer")
					require.Equal(t, 1, upstream.calls())
					require.Len(t, logs.created, 1)
					require.Len(t, billing.commands, 1)
					row := <-logs.created
					if tc.wantUsage {
						require.Equal(t, 10, row.InputTokens)
						require.Equal(t, 5, row.OutputTokens)
						require.Positive(t, row.ActualCost)
						require.Positive(t, billing.commands[0].BalanceCost)
					} else {
						require.Zero(t, row.TotalTokens())
						require.Zero(t, row.ActualCost)
						require.Zero(t, billing.commands[0].BalanceCost)
					}
				})
			}
		}
	}
}

func TestHTTPContinuationSkipsChatOnlyAccount(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprintf("accounts=%d", count), func(t *testing.T) {
			accounts := preContentAccounts(count)
			accounts[0].Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions)}
			rec, up := serveMessagesScenario(t, preContentFailoverScenario{accounts: accounts, endpoint: "/v1/responses", ownedResponse: "resp_parent", requestBody: `{"model":"gpt-5.4","input":"what was the number?","previous_response_id":"resp_parent","stream":true}`, respond: func(int) (*http.Response, error) {
				return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentHandlerSuccess))), nil
			}})
			if count == 1 {
				require.Zero(t, up.calls())
				require.Equal(t, 400, rec.Code)
				require.Contains(t, rec.Body.String(), "resend the full conversation")
				return
			}
			require.Equal(t, []int64{accounts[1].ID}, up.hits)
			require.Equal(t, "resp_parent", gjson.Get(up.bodies[0], "previous_response_id").String())
			require.Contains(t, rec.Body.String(), "answer from second account")
		})
	}
}

func TestOpenAIPluginReadErrorRetryCount(t *testing.T) {
	for _, sent := range []bool{true, false} {
		t.Run(fmt.Sprintf("sent=%t", sent), func(t *testing.T) {
			rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{accounts: preContentAccounts(2), endpoint: "/v1/chat/completions", requestBody: `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":false}`, respond: func(call int) (*http.Response, error) {
				if call == 1 {
					err := &service.PluginTransportError{RequestSent: sent, Code: "read_failed", Message: "interrupted"}
					if !sent {
						return nil, err
					}
					return preContentSSEResponse(io.NopCloser(iotest.ErrReader(err))), nil
				}
				return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentHandlerSuccess))), nil
			}})
			if sent {
				require.Equal(t, 1, upstream.calls())
				require.GreaterOrEqual(t, rec.Code, 400)
			} else {
				require.Equal(t, 2, upstream.calls())
				require.Equal(t, 200, rec.Code)
			}
		})
	}
}

// requireEndsWithSSEError checks that a response already committed as SSE ends
// with an Anthropic error event and never carries a bare JSON line.
func requireEndsWithSSEError(t *testing.T, body string) {
	t.Helper()
	lastEvent := body[strings.LastIndex(strings.TrimRight(body, "\n"), "\n\n")+2:]
	require.True(t, strings.HasPrefix(lastEvent, "event: error\ndata: {"), "last event: %q", lastEvent)
	require.Contains(t, lastEvent, `"type":"error"`)
	for _, line := range strings.Split(body, "\n") {
		require.False(t, strings.HasPrefix(line, "{"), "bare JSON line in SSE body: %q", line)
	}
}

func TestOpenAIMessagesFailsOverAfterKeepaliveOnlyWrite(t *testing.T) {
	rec, upstream := servePreContentFailoverMessages(t, 2)

	body := rec.Body.String()
	require.Equal(t, 2, upstream.calls(), "body: %q", body)
	require.Equal(t, 1, strings.Count(body, "event: message_start"), "body: %q", body)
	require.Contains(t, body, "answer from second account")
}

func TestOpenAIMessagesKeepaliveThenFailoverExhaustedEndsWithSSEError(t *testing.T) {
	rec, upstream := servePreContentFailoverMessages(t, 1)

	body := rec.Body.String()
	require.Equal(t, 1, upstream.calls(), "body: %q", body)
	require.Contains(t, body, "event: ping")
	requireEndsWithSSEError(t, body)
}

func TestOpenAIMessagesFailoverThenSecondAccountReadErrorEndsWithSSEError(t *testing.T) {
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts: preContentAccounts(2),
		respond: func(call int) (*http.Response, error) {
			if call == 1 {
				return preContentStallThenFail(preContentHandlerFailed), nil
			}
			return preContentSSEResponse(io.NopCloser(unexpectedEOFReader{})), nil
		},
	})

	body := rec.Body.String()
	require.Equal(t, 2, upstream.calls(), "body: %q", body)
	require.NotContains(t, body, "message_start")
	requireEndsWithSSEError(t, body)
}

func TestOpenAIMessagesFailoverThenSecondAccountCredentialErrorEndsWithSSEError(t *testing.T) {
	accounts := preContentAccounts(2)
	accounts[1].Credentials = map[string]any{"base_url": "http://upstream.example"}
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts: accounts,
		respond: func(call int) (*http.Response, error) {
			return preContentStallThenFail(preContentHandlerFailed), nil
		},
	})

	body := rec.Body.String()
	require.Equal(t, 1, upstream.calls(), "body: %q", body)
	require.NotContains(t, body, "message_start")
	requireEndsWithSSEError(t, body)
}

func TestOpenAIMessagesSlotAcquireFailureAfterKeepaliveIsSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Header("Content-Type", "text/event-stream")
	_, err := io.WriteString(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	require.NoError(t, err)
	streamStarted := true

	h := &OpenAIGatewayHandler{}
	selection := &service.AccountSelectionResult{Account: &service.Account{ID: 9201}}
	_, result := h.acquireResponsesAccountSlot(c, nil, "", selection, true, &streamStarted, zap.NewNop())

	require.Equal(t, openAISlotAcquireFailed, result)
	body := rec.Body.String()
	lastEvent := body[strings.LastIndex(strings.TrimRight(body, "\n"), "\n\n")+2:]
	require.True(t, strings.HasPrefix(lastEvent, "event: error\ndata: {"), "last event: %q", lastEvent)
}

func newFailedFrameCodeRule(code string) *service.ErrorPassthroughService {
	status := http.StatusTeapot
	rule := &model.ErrorPassthroughRule{
		ID: 1, Name: "frame code rule", Enabled: true, Priority: 1,
		Keywords: []string{code}, MatchMode: model.MatchModeAny,
		Platforms: []string{model.PlatformOpenAI}, ResponseCode: &status, PassthroughBody: true,
	}
	return service.NewErrorPassthroughService(&errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{rule}}, nil)
}

// A failed frame that triggers a failover is no longer written in-band, so its
// passthrough rule has to apply once the failover is exhausted. The rule is
// keyed on the frame's own code, which the generic failover envelope lacks.
func TestOpenAIMessagesFailoverExhaustedAppliesFailedFrameRule(t *testing.T) {
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts:    preContentAccounts(1),
		passthrough: newFailedFrameCodeRule("rate_limit_exceeded"),
		respond: func(call int) (*http.Response, error) {
			failed := `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5.4"}}}` + "\n\n"
			return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentHandlerCreated + failed))), nil
		},
	})

	body := rec.Body.String()
	t.Logf("status=%d body=%q", rec.Code, body)
	require.Equal(t, 1, upstream.calls(), "body: %q", body)
	require.Equal(t, http.StatusTeapot, rec.Code, "body: %q", body)
	require.Equal(t, "upstream_error", gjson.Get(body, "error.type").String(), "body: %q", body)
	require.Equal(t, "Rate limit reached for gpt-5.4", gjson.Get(body, "error.message").String(), "body: %q", body)
}

const preContentRateLimitFailed = `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5.4"}}}` + "\n\n"

func requireFailedFrameRuleJSON(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	t.Logf("status=%d content-type=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), body)
	require.Equal(t, http.StatusTeapot, rec.Code, "body: %q", body)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.Equal(t, "upstream_error", gjson.Get(body, "error.type").String(), "body: %q", body)
	require.Equal(t, "Rate limit reached for gpt-5.4", gjson.Get(body, "error.message").String(), "body: %q", body)
}

// The failed frame is the first event of the stream, so it triggers the
// failover before any output and used to reach the generic mapping unchecked.
func TestOpenAIMessagesFirstEventFailedFrameExhaustedAppliesRule(t *testing.T) {
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts:    preContentAccounts(1),
		passthrough: newFailedFrameCodeRule("rate_limit_exceeded"),
		respond: func(call int) (*http.Response, error) {
			return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentRateLimitFailed))), nil
		},
	})

	require.Equal(t, 1, upstream.calls())
	requireFailedFrameRuleJSON(t, rec)
}

func TestOpenAIMessagesNonStreamFailedFrameExhaustedAppliesRule(t *testing.T) {
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts:    preContentAccounts(1),
		passthrough: newFailedFrameCodeRule("rate_limit_exceeded"),
		requestBody: `{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
		respond: func(call int) (*http.Response, error) {
			return preContentSSEResponse(io.NopCloser(strings.NewReader(preContentHandlerCreated + preContentRateLimitFailed))), nil
		},
	})

	require.Equal(t, 1, upstream.calls())
	requireFailedFrameRuleJSON(t, rec)
}

// A failover triggered by an HTTP status keeps the built-in exhaustion mapping:
// no passthrough rule is looked up for it.
func TestOpenAIMessagesHTTPStatusFailoverExhaustedIgnoresPassthroughRule(t *testing.T) {
	rec, upstream := serveMessagesScenario(t, preContentFailoverScenario{
		accounts:    preContentAccounts(1),
		passthrough: newFailedFrameCodeRule("rate_limit_exceeded"),
		respond: func(call int) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5.4"}}`)),
			}, nil
		},
	})

	body := rec.Body.String()
	t.Logf("status=%d body=%q", rec.Code, body)
	require.Equal(t, 1, upstream.calls(), "body: %q", body)
	require.NotEqual(t, http.StatusTeapot, rec.Code, "body: %q", body)
	require.Equal(t, "Upstream service temporarily unavailable", gjson.Get(body, "error.message").String(), "body: %q", body)
}
