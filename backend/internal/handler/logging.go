package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func requestLogger(c *gin.Context, component string, fields ...zap.Field) *zap.Logger {
	base := logger.L()
	if c != nil && c.Request != nil {
		ctx := c.Request.Context()
		base = logger.FromContext(ctx)
		if platform := resolveOpsPlatform(ctx, getOpsAPIKey(c), ""); platform != "" {
			base = base.With(zap.String("platform", platform))
		}
	}

	if component != "" {
		fields = append([]zap.Field{zap.String("component", component)}, fields...)
	}
	return base.With(fields...)
}
