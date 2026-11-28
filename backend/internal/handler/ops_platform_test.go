package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveOpsPlatformPrefersResolvedCompositeTarget(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	ctx := service.WithResolvedTargetPlatform(context.Background(), service.PlatformOpenAI)

	require.Equal(t, service.PlatformOpenAI, resolveOpsPlatform(ctx, apiKey, service.PlatformAnthropic))
}

func TestRequestLoggerRecordsOpsPlatform(t *testing.T) {
	sink, restore := captureHandlerStructuredLog(t)
	defer restore()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
	requestLogger(c, "handler.openai_gateway.images").Warn("before selecting image account")
	field, found := sink.FieldValueForMessage("before selecting image account", "platform")
	require.True(t, found)
	require.Equal(t, service.PlatformOpenAI, field)

	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}})
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformAnthropic))
	requestLogger(c, "handler.openai_gateway.responses").Warn("after resolving composite target")
	field, found = sink.FieldValueForMessage("after resolving composite target", "platform")
	require.True(t, found)
	require.Equal(t, service.PlatformAnthropic, field)
}
