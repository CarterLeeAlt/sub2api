package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
)

// abortIfRequestCanceled checks the HTTP request context, which outlives any
// operation-specific context used by a database query or an upstream call.
func abortIfRequestCanceled(c *gin.Context) bool {
	if c.Request.Context().Err() != context.Canceled {
		return false
	}
	c.AbortWithStatus(httputil.StatusClientClosedRequest)
	return true
}
