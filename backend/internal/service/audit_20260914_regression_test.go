//go:build unit

package service

// 2026-09-14 审查修复轮的回归测试。每个测试对应 FORK_NOTES"2026-09-14 全面
// 审查修复轮"中的一条语义决策，防止后续改动无意回退。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// 回归④（P3-8）：批量生图仅支持按张计费——token 计价不得作为每张单价返回。
// ---------------------------------------------------------------------------

// 危险形态的存在性证明：渠道 token 计费 + 显式 image output 价会让
// ResolvedPricing 携带 token 粒度的图片输出价（历史上 BatchImageUnitPrice
// 的 token 分支把它当"每张单价"返回，估价/冻结/结算全链路近乎免费出图）。
func TestBatchImageUnitPrice_TokenBillingModelIsRejected(t *testing.T) {
	_, resolver := newTokenCostTestEnv(t, "gemini", []ChannelModelPricing{{
		Platform:         "gemini",
		Models:           []string{"token-image-model"},
		BillingMode:      BillingModeToken,
		ImageOutputPrice: float64Ptr(4e-5), // $40 / 1M tokens
	}}, nil)
	groupID := int64(100)
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "token-image-model", GroupID: &groupID})
	require.Equal(t, BillingModeToken, resolved.Mode, "前置：渠道 token 计费配置真实生效")
	require.NotNil(t, resolved.BasePricing)
	require.True(t, resolved.BasePricing.ImageOutputPriceExplicit, "前置：显式 image per-token 价真实存在")
	require.Greater(t, resolved.BasePricing.ImageOutputPricePerToken, 0.0)

	// 语义锁：无论形态如何，BatchImageUnitPrice 对 token 计费模型必须拒绝，
	// 永远不得把每 token 价当每张价返回。
	pricer := &BatchImageModelPricingResolver{Resolver: resolver}
	unit, err := pricer.BatchImageUnitPrice(context.Background(), &BatchImageJob{
		Provider: BatchImageProviderGeminiAPI,
		Model:    "token-image-model",
	})
	require.ErrorIs(t, err, ErrBatchImageSettlementPricingMissing)
	require.Equal(t, 0.0, unit)
}

// ---------------------------------------------------------------------------
// 回归⑤（P2-3）：WS ingress 的生图 turn 必须占 ImageConcurrency 槽位，
// turn 收尾释放；占槽失败以"生图并发超限"关闭连接。
// ---------------------------------------------------------------------------

type imageSlotRecorder struct {
	acquired atomic.Int32
	released atomic.Int32
	deny     atomic.Bool
}

func (r *imageSlotRecorder) acquire(_ int) (func(), bool) {
	if r.deny.Load() {
		return nil, false
	}
	r.acquired.Add(1)
	return func() { r.released.Add(1) }, true
}

func newImageSlotIngressService(t *testing.T) (*OpenAIGatewayService, *Account, *openAIWSDrainProbeConn) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	upstreamConn := &openAIWSDrainProbeConn{
		gate:   make(chan struct{}),
		event1: []byte(`{"type":"response.created","response":{"id":"resp_img_slot"}}`),
		event2: []byte(`{"type":"response.completed","response":{"id":"resp_img_slot","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstreamConn})
	t.Cleanup(pool.Close)

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := &Account{
		ID:          3115,
		Name:        "openai-ingress-image-slot",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	return svc, account, upstreamConn
}

func runImageSlotIngressTurn(t *testing.T, svc *OpenAIGatewayService, account *Account, probe *openAIWSDrainProbeConn, slots *imageSlotRecorder, firstMessage []byte) error {
	t.Helper()
	serverErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		req := r.Clone(r.Context())
		ginCtx.Request = req
		group := &Group{AllowImageGeneration: true}
		ginCtx.Set("api_key", &APIKey{ID: 778, UserID: 55, Group: group})

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, first, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		hooks := &OpenAIWSIngressHooks{ImageSlotAcquire: slots.acquire}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		serverErrCh <- svc.ProxyResponsesWebSocketFromClient(ctx, ginCtx, conn, account, "sk-test", first, hooks)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	writeErr := clientConn.Write(writeCtx, coderws.MessageText, firstMessage)
	cancelWrite()
	require.NoError(t, writeErr)

	// 读到 completed 即 turn 正常收尾；占槽失败时服务端会直接关连接（读到错误）。
	readCtx, cancelRead := context.WithTimeout(context.Background(), 4*time.Second)
	for {
		_, frame, readErr := clientConn.Read(readCtx)
		if readErr != nil {
			break
		}
		frameType := gjson.GetBytes(frame, "type").String()
		if frameType == "response.created" {
			// 放行上游 probe 的终端事件，让 sendAndRelay 正常收尾。
			select {
			case <-probe.gate:
			default:
				close(probe.gate)
			}
		}
		if frameType == "response.completed" {
			break
		}
	}
	cancelRead()
	_ = clientConn.CloseNow()

	select {
	case err := <-serverErrCh:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("等待 ingress turn 结束超时")
		return nil
	}
}

func TestOpenAIWSIngressProxy_ImageTurnAcquiresAndReleasesSlot(t *testing.T) {
	svc, account, probe := newImageSlotIngressService(t)
	slots := &imageSlotRecorder{}

	// 显式 image_generation 工具声明 → 生图意图 → sendAndRelay 前必须占槽。
	firstMessage := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"draw a cat"}],"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	err := runImageSlotIngressTurn(t, svc, account, probe, slots, firstMessage)
	require.NoError(t, err)
	require.Equal(t, int32(1), slots.acquired.Load(), "生图 turn 必须恰好占槽一次")
	require.Equal(t, int32(1), slots.released.Load(), "turn 收尾后必须释放槽位")
}

func TestOpenAIWSIngressProxy_NonImageTurnDoesNotTouchSlot(t *testing.T) {
	svc, account, probe := newImageSlotIngressService(t)
	slots := &imageSlotRecorder{}

	firstMessage := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"hi"}]}`)
	err := runImageSlotIngressTurn(t, svc, account, probe, slots, firstMessage)
	require.NoError(t, err)
	require.Equal(t, int32(0), slots.acquired.Load(), "非生图 turn 不得占生图槽")
	require.Equal(t, int32(0), slots.released.Load())
}

func TestOpenAIWSIngressProxy_ImageSlotExhaustedClosesTurn(t *testing.T) {
	svc, account, probe := newImageSlotIngressService(t)
	slots := &imageSlotRecorder{}
	slots.deny.Store(true)

	firstMessage := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"draw a cat"}],"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	err := runImageSlotIngressTurn(t, svc, account, probe, slots, firstMessage)
	require.Error(t, err)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Contains(t, err.Error(), "Image generation concurrency limit exceeded")
	require.Equal(t, int32(0), slots.released.Load(), "占槽失败时不得触发 release")
}

// ---------------------------------------------------------------------------
// 回归②（P2-5）：ctx_pool 429 failover 错误的 current-turn 包装协议。
//
// 完整的双轮 WS 编排需要 handler 层 failover 循环参与，这里在 service 层
// 锁定 handler 依赖的协议契约：sendAndRelay 返回的裸 429 failover 错误
// 不携带重放载荷；经主循环的 newOpenAIWSCurrentTurnFailoverError 包装后
// handler 必须能提取当前轮载荷，且 errors.As(*UpstreamFailoverError) 链
// 保持完整。
// ---------------------------------------------------------------------------

func TestOpenAIWSIngressProxy_SecondTurn429CurrentTurnRetryPayloadProtocol(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// sendAndRelay 429 分支的裸错误形态（不含 current-turn 载荷）。
	bare := svc.newOpenAIWSRateLimitFailoverError(
		&Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		nil, []byte(`{"type":"error","code":"rate_limit_exceeded"}`), "rate limited",
	)
	require.NotNil(t, bare)
	_, hasPayload := OpenAIWSCurrentTurnRetryPayload(bare)
	require.False(t, hasPayload, "裸 429 failover 错误不应自带重放载荷")

	// 主循环的包装形态：载荷为当前轮的账号无关 response.create 帧。
	currentTurnPayload := []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"turn two"}]}`)
	wrapped := newOpenAIWSCurrentTurnFailoverError(bare, currentTurnPayload)
	payload, hasPayload := OpenAIWSCurrentTurnRetryPayload(wrapped)
	require.True(t, hasPayload, "包装后 handler 必须能提取当前轮重放载荷")
	require.Equal(t, string(currentTurnPayload), string(payload))
	require.Contains(t, string(payload), "turn two")

	// Unwrap 链保持：handler 的 errors.As(*UpstreamFailoverError) 仍能命中，
	// 429 状态码与账号侧效应处理不受包装影响。
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, wrapped, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
}

// ---------------------------------------------------------------------------
// 回归③（P2-6）：passthrough 零输出（bare error 被抑制）断流时，收尾写
// 终态 response.failed 前必须停拍心跳——终态事件字节不得被心跳注释行
// 插入（写竞态的确定性观测面）。
// ---------------------------------------------------------------------------

func TestPassthroughStreaming_ZeroOutputBareErrorTerminalHasNoKeepaliveInterleave(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// bare error 延迟 1.5s 到达：期间 keepalive（1s 间隔）至少 beat 一次，
	// 流 EOF 后主循环退出——修复前 ensureResponseFailedTerminal 与仍在运行
	// 的心跳并发写同一 writer，注释行可能插进终态事件字节之间。
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"upstream died\"}\n\n"))
		_ = pw.Close()
	}()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// failAfterWrites=-1：全部写成功，聚焦"终态与心跳的交错"而非下游断连。
	writer := &passthroughFlushTestWriter{ResponseWriter: c.Writer, recorder: recorder, failAfterWrites: -1}
	c.Writer = writer

	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize:              defaultMaxLineSize,
		StreamKeepaliveInterval:  1,
		StreamDataIntervalTimeout: 0,
	}}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Name: "keepalive-terminal-test"}

	done := make(chan struct{})
	var result *openaiStreamingResultPassthrough
	var err error
	go func() {
		defer close(done)
		result, err = svc.handleStreamingResponsePassthrough(
			context.Background(), resp, c, account, time.Now(), "", "")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("流式处理未返回（疑似心跳 goroutine 死锁）")
	}
	// 上游失败照常向上传播（failover/账号侧判定依赖它），但客户端必须已经
	// 收到完整的 response.failed 终态。
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream response failed")
	require.NotNil(t, result)

	body := recorder.Body.String()
	require.Contains(t, body, "response.failed", "零输出 bare error 必须由终态兜底写出 response.failed")
	// 最后一个 data 事件必须是完整闭合的 response.failed，事件内部不得混入
	// 心跳注释行（修复后的停拍时序保证：注释行全部先于终态字节落盘，二者
	// 无交错——并发写竞态的确定性观测面）。
	lastData := strings.LastIndex(body, "data: {")
	require.GreaterOrEqual(t, lastData, 0)
	terminal := body[lastData:]
	require.True(t, strings.HasSuffix(terminal, "\n\n"), "终态事件必须完整闭合")
	require.NotContains(t, terminal, "\n:\n", "终态事件字节之间不得插入心跳注释行")
	require.Contains(t, terminal, "\"type\":\"response.failed\"", "最后一个 data 事件必须是 response.failed")
}

// ---------------------------------------------------------------------------
// 回归（审计轮追加）：bindHTTPResponseAccount 在响应写回客户端之后执行，
// 非流式请求的客户端此刻往往已断开、原始请求 ctx 已取消——用取消的 ctx 写
// Redis 绑定必然失败（生产观测 openai.http_bind_response_owner_failed:
// context canceled）。bind 是断连后仍必须完成的收尾动作：粘性丢失会让
// previous_response_id 续链退回普通调度，owner 绑定丢失会让后续请求直接
// 400。修复：入口脱离请求取消（WithoutCancel）并给 3s 显式预算。
// ---------------------------------------------------------------------------

type ctxRecordingGatewayCache struct {
	stubGatewayCache
	mu                sync.Mutex
	setAccountCtxErrs []error
}

func (c *ctxRecordingGatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	var err error
	if ctx != nil {
		err = ctx.Err()
	}
	c.mu.Lock()
	c.setAccountCtxErrs = append(c.setAccountCtxErrs, err)
	c.mu.Unlock()
	return c.stubGatewayCache.SetSessionAccountID(ctx, groupID, sessionHash, accountID, ttl)
}

func TestBindHTTPResponseAccount_SurvivesCanceledRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &ctxRecordingGatewayCache{}
	svc := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(cache)}

	groupID := int64(100)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 900, UserID: 55, GroupID: &groupID})
	c.Set(openAIHTTPResponseOwnerContextKey, openAIHTTPResponseOwner{userID: 55, apiKeyID: 900})

	// 模拟"响应已写回、客户端立即断开"：请求 ctx 已取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.bindHTTPResponseAccount(ctx, c, &Account{ID: 43, Platform: PlatformOpenAI}, "resp_bind_survive")

	cache.mu.Lock()
	errs := append([]error(nil), cache.setAccountCtxErrs...)
	cache.mu.Unlock()
	require.NotEmpty(t, errs, "账号绑定写必须被执行")
	for i, err := range errs {
		require.NoError(t, err, "第 %d 次绑定写必须使用脱离请求取消的 ctx", i+1)
	}

	// 本地绑定可读回：粘性与续链鉴权的数据已就位。
	got, err := svc.openaiWSStateStore.GetResponseAccount(context.Background(), groupID, "resp_bind_survive")
	require.NoError(t, err)
	require.Equal(t, int64(43), got)
}
