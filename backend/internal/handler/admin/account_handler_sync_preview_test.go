package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type syncPreviewRecordingUpstream struct {
	request *http.Request
}

func (u *syncPreviewRecordingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.request = req.Clone(req.Context())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"model-a"}]}`)),
		Request:    req,
	}, nil
}

func (u *syncPreviewRecordingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func setupSyncUpstreamModelsPreviewRouter(upstream service.HTTPUpstream) *gin.Engine {
	gin.SetMode(gin.TestMode)
	accountTestSvc := service.NewAccountTestService(
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		&config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		nil,
	)
	handler := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, accountTestSvc, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/accounts/models/sync-upstream-preview", handler.SyncUpstreamModelsPreview)
	return router
}

func TestAccountHandlerSyncUpstreamModelsPreviewPreservesCNProtocolAndMode(t *testing.T) {
	upstream := &syncPreviewRecordingUpstream{}
	router := setupSyncUpstreamModelsPreviewRouter(upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/models/sync-upstream-preview", strings.NewReader(`{
		"platform":"zhipu",
		"type":"apikey",
		"base_url":"https://open.bigmodel.cn/api/anthropic",
		"api_key":"sk-test",
		"account_mode":"coding",
		"api_protocol":"anthropic"
	}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, upstream.request)
	require.Equal(t, "https://open.bigmodel.cn/api/coding/paas/v4/models", upstream.request.URL.String())
	require.Contains(t, rec.Body.String(), "model-a")
}
