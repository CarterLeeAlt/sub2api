package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/imroc/req/v3"
)

// req buffers response bodies before returning. Keep its built-in retries off
// at call sites so the shared read policy owns the entire attempt budget.
func readReqResponse(ctx context.Context, timeout time.Duration, send func(context.Context) (*req.Response, error)) (*req.Response, error) {
	var response *req.Response
	err := httpclient.RetryRead(ctx, timeout, func(attemptCtx context.Context) (int, http.Header, error) {
		var err error
		response, err = send(attemptCtx)
		if response == nil || response.Response == nil {
			return 0, nil, err
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return response.StatusCode, response.Header, err
	})
	return response, err
}
