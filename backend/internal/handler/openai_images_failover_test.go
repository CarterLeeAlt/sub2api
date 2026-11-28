//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type openAIImagesFailoverAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r openAIImagesFailoverAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) accountsForPlatform(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out
}

type openAIImagesFailoverHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	models     []string
	response   string
}

func (u *openAIImagesFailoverHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	var model string
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		model = gjson.GetBytes(body, "model").String()
	}
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.models = append(u.models, model)
	u.mu.Unlock()
	response := u.response
	if response == "" {
		response = `{"error":{"type":"server_error","code":"server_error","message":"image backend unavailable"}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req_img_failover"},
		},
		Body: io.NopCloser(strings.NewReader(response)),
	}, nil
}

func (u *openAIImagesFailoverHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func TestOpenAIGatewayHandlerImages_RequiresModelAndOnlySwitchesAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sunburst = "gpt-image-2.6-sunburst"
	const flare = "gpt-image-2.6-flare"
	newAccount := func(id int64, model string) service.Account {
		account := newOpenAIImagesSizeRoutingAccount(id, "1K", "2K", "4K")
		account.Credentials["model_mapping"] = map[string]any{model: model}
		service.SetAccountModelCatalog(&account, service.AccountModelCatalog{Source: "upstream", Ready: true})
		return account
	}
	for _, tc := range []struct {
		name       string
		body       string
		accounts   []service.Account
		wantStatus int
		wantIDs    []int64
		wantModels []string
	}{
		{
			name:       "Sunburst only switches between its accounts",
			body:       `{"model":"gpt-image-2.6-sunburst","prompt":"draw"}`,
			accounts:   []service.Account{newAccount(1, flare), newAccount(2, sunburst), newAccount(3, sunburst)},
			wantStatus: http.StatusBadGateway,
			wantIDs:    []int64{2, 3},
			wantModels: []string{sunburst, sunburst},
		},
		{
			name:       "missing model is rejected even when an account is available",
			body:       `{"prompt":"draw"}`,
			accounts:   []service.Account{newAccount(1, flare)},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "explicit Flare selects Flare even when Sunburst is available",
			body:       `{"model":"gpt-image-2.6-flare","prompt":"draw"}`,
			accounts:   []service.Account{newAccount(1, sunburst), newAccount(2, flare)},
			wantStatus: http.StatusBadGateway,
			wantIDs:    []int64{2},
			wantModels: []string{flare},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &openAIImagesFailoverHTTPUpstream{}
			handler := newOpenAIImagesSizeRoutingTestHandler(t, tc.accounts, upstream)
			response, _ := serveOpenAIImagesSizeRoutingRequest(t.Context(), handler, 3144, tc.body)
			require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			require.Equal(t, tc.wantIDs, upstream.calls())
			require.Equal(t, tc.wantModels, upstream.models)
			if tc.wantStatus == http.StatusBadRequest {
				require.Equal(t, "invalid_request_error", gjson.GetBytes(response.Body.Bytes(), "error.type").String())
				require.Equal(t, "model is required", gjson.GetBytes(response.Body.Bytes(), "error.message").String())
			}
		})
	}
}

func TestOpenAIGatewayHandlerImages_ServerErrorFailsOverAndReturnsClearErrorWhenExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3130)
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "image-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		{
			ID:          2,
			Name:        "image-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	upstream := &openAIImagesFailoverHTTPUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	body := []byte(`{"model":"gpt-image-1","prompt":"draw a cat","quality":"high","size":"1536x1024"}`)
	core, observedLogs := observer.New(zap.DebugLevel)
	requestCtx := logger.IntoContext(context.Background(), zap.New(core))
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: true,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})

	handler.Images(c)

	accountSelectingLogs := observedLogs.FilterMessage("openai.images.account_selecting").All()
	require.NotEmpty(t, accountSelectingLogs)
	loggedFields := make(map[string]string)
	for _, field := range accountSelectingLogs[0].Context {
		loggedFields[field.Key] = field.String
	}
	require.Equal(t, "high", loggedFields["img_quality"])
	require.Equal(t, "1536x1024", loggedFields["img_size"])
	require.NotContains(t, loggedFields, "prompt")

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream service temporarily unavailable", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())

	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, "failover", events[1].Kind)
}
