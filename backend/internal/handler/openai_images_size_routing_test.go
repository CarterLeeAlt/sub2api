//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

type openAIImagesSizeRoutingAccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (r openAIImagesSizeRoutingAccountRepo) ListActive(context.Context) ([]service.Account, error) {
	return r.accounts, nil
}

func (r openAIImagesSizeRoutingAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	var out []service.Account
	for _, account := range r.accountsForPlatform(platform) {
		if account.IsSchedulable() {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r openAIImagesSizeRoutingAccountRepo) ListModelAvailabilityCandidates(_ context.Context, _ *int64, platforms []string, _ bool) ([]service.Account, error) {
	var out []service.Account
	for _, platform := range platforms {
		for _, account := range r.accountsForPlatform(platform) {
			if account.IsActive() && account.Schedulable {
				out = append(out, account)
			}
		}
	}
	return out, nil
}

func newOpenAIImagesSizeRoutingTestHandler(t *testing.T, accounts []service.Account, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesSizeRoutingAccountRepo{openAIImagesFailoverAccountRepo{accounts: accounts}},
		nil, nil, nil, nil, nil, nil,
		cfg,
		nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

func serveOpenAIImagesSizeRoutingRequest(ctx context.Context, handler *OpenAIGatewayHandler, groupID int64, body string) (*httptest.ResponseRecorder, *gin.Context) {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, AllowImageGeneration: true},
		User:    &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	handler.Images(c)
	return rec, c
}

func newOpenAIImagesSizeRoutingAccount(id int64, tiers ...any) service.Account {
	return service.Account{
		ID:          id,
		Name:        fmt.Sprintf("image-size-account-%d", id),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    int(id),
		Credentials: map[string]any{"access_token": fmt.Sprintf("token-%d", id)},
		Extra:       map[string]any{service.AccountExtraOpenAIImageSizeTiers: tiers},
	}
}

func TestOpenAIGatewayHandlerFirstSelectionClassifiesUnavailableAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		mutate     func(*service.Account)
		wantStatus int
		wantType   string
		poolReason string
	}{
		{"manually paused", func(a *service.Account) { a.Schedulable = false }, http.StatusServiceUnavailable, "api_error", "manually_paused"},
		{"rate limited", func(a *service.Account) {
			until := time.Now().Add(time.Hour)
			a.RateLimitResetAt = &until
		}, http.StatusTooManyRequests, "rate_limit_error", "account_rate_limited"},
		{"unsupported model", func(a *service.Account) {
			a.Credentials["model_mapping"] = map[string]any{"gpt-6.2-astra": "gpt-6.2-astra"}
		}, http.StatusNotFound, "model_not_found", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newOpenAIImagesSizeRoutingAccount(1, "2K")
			tc.mutate(&account)
			upstream := &openAIImagesFailoverHTTPUpstream{}
			handler := newOpenAIImagesSizeRoutingTestHandler(t, []service.Account{account}, upstream)
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.IntoContext(t.Context(), zap.New(core))
			image, _ := serveOpenAIImagesSizeRoutingRequest(ctx, handler, 3140, `{"model":"gpt-image-2.6-sunburst","prompt":"draw","size":"1536x1024"}`)
			text, _ := serveOpenAIResponsesSizeRoutingRequest(ctx, handler, 3140, `{"model":"gpt-6.2-sol","input":"test"}`)
			for _, response := range []*httptest.ResponseRecorder{image, text} {
				require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
				require.Equal(t, tc.wantType, gjson.GetBytes(response.Body.Bytes(), "error.type").String())
			}
			require.Empty(t, upstream.calls())
			for _, message := range []string{"openai.images.account_select_failed", "openai.account_select_failed"} {
				entries := logs.FilterMessage(message).All()
				require.Len(t, entries, 1)
				fields := entries[0].ContextMap()
				if tc.poolReason == "" {
					require.NotContains(t, fields, "account_pool_db_state")
					continue
				}
				require.Equal(t, map[string]int{"active_accounts": 1, tc.poolReason: 1}, fields["account_pool_db_state"])
			}
		})
	}
}

func TestOpenAIGatewayHandlerImages_SizeTierMissingFromGroupReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesFailoverHTTPUpstream{}
	handler := newOpenAIImagesSizeRoutingTestHandler(t, []service.Account{
		newOpenAIImagesSizeRoutingAccount(1, "1K", "2K"),
	}, upstream)

	rec, c := serveOpenAIImagesSizeRoutingRequest(t.Context(), handler, 3140, `{"model":"gpt-image-2","prompt":"draw a cat","size":"3840x2160"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "4K")
	require.Empty(t, upstream.calls())
	reason, _ := c.Get(service.OpsClientBusinessLimitedReasonKey)
	require.Equal(t, service.OpsClientBusinessLimitedReasonLocalModelConfiguration, reason)
}

func TestOpenAIGatewayHandlerImages_SizeTierRoutesToAllowedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesFailoverHTTPUpstream{}
	handler := newOpenAIImagesSizeRoutingTestHandler(t, []service.Account{
		newOpenAIImagesSizeRoutingAccount(1, "1K", "2K"),
		newOpenAIImagesSizeRoutingAccount(2, "4K"),
		newOpenAIImagesSizeRoutingAccount(3, "4K"),
	}, upstream)

	rec, _ := serveOpenAIImagesSizeRoutingRequest(t.Context(), handler, 3141, `{"model":"gpt-image-1","prompt":"draw a cat","size":"2880x2880"}`)

	require.Equal(t, http.StatusBadGateway, rec.Code, "the stub upstream fails every attempt")
	require.Equal(t, []int64{2, 3}, upstream.calls(), "4K requests fail over only across 4K accounts")
}

func serveOpenAIResponsesSizeRoutingRequest(ctx context.Context, handler *OpenAIGatewayHandler, groupID int64, body string) (*httptest.ResponseRecorder, *gin.Context) {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true},
		User:    &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	handler.Responses(c)
	return rec, c
}

func TestOpenAIGatewayHandlerResponses_ImageToolSizeTierMissingFromGroupReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesFailoverHTTPUpstream{}
	handler := newOpenAIImagesSizeRoutingTestHandler(t, []service.Account{
		newOpenAIImagesSizeRoutingAccount(1, "1K", "2K"),
	}, upstream)

	rec, _ := serveOpenAIResponsesSizeRoutingRequest(t.Context(), handler, 3142, `{"model":"gpt-5.4","stream":false,"input":"draw a cat","tools":[{"type":"image_generation","size":"3840x2160"}]}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "4K")
	require.Empty(t, upstream.calls())
}

func TestOpenAIGatewayHandlerResponses_TextRequestIgnoresImageSizeTiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesFailoverHTTPUpstream{}
	handler := newOpenAIImagesSizeRoutingTestHandler(t, []service.Account{
		newOpenAIImagesSizeRoutingAccount(1, "4K"),
	}, upstream)

	_, _ = serveOpenAIResponsesSizeRoutingRequest(t.Context(), handler, 3143, `{"model":"gpt-5.4","stream":false,"input":"hello","size":"1024x1024"}`)

	require.NotEmpty(t, upstream.calls())
	require.Equal(t, int64(1), upstream.calls()[0])
}
