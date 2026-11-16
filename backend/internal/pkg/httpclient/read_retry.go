package httpclient

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var ErrReadResponseTooLarge = errors.New("read response too large")
var errInvalidReadResponse = errors.New("read transport returned no response body")

// RetryRead is opt-in for read-only operations, including read-only POSTs.
// The callback must consume and close its response. Each of the two attempts
// has its own deadline within the caller's total budget. Mutations and streams
// must not use this function.
func RetryRead(ctx context.Context, attemptTimeout time.Duration, read func(context.Context) (int, http.Header, error)) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		status, headers, err := read(attemptCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == 1 || !retryableRead(status, err) {
			return err
		}
		delay := 200*time.Millisecond + time.Duration(rand.Int64N(int64(200*time.Millisecond)))
		if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
			var hint time.Duration
			if seconds, parseErr := strconv.ParseInt(raw, 10, 32); parseErr == nil && seconds >= 0 {
				hint = time.Duration(seconds) * time.Second
			} else if date, parseErr := http.ParseTime(raw); parseErr == nil {
				hint = time.Until(date)
			} else if status == http.StatusTooManyRequests {
				return err
			}
			// Longer cooldowns belong to the background scheduler. Never retry
			// earlier than the upstream permits.
			if hint > 2*time.Second {
				return err
			}
			if hint > delay {
				delay = hint
			}
		} else if status == http.StatusTooManyRequests {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func retryableRead(status int, err error) bool {
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return false
	}
	if err != nil {
		var unknownCA x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalidCert x509.CertificateInvalidError
		return !errors.Is(err, context.Canceled) && !errors.Is(err, ErrReadResponseTooLarge) && !errors.Is(err, errInvalidReadResponse) &&
			!errors.As(err, &unknownCA) && !errors.As(err, &hostname) && !errors.As(err, &invalidCert)
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// DoRead buffers a read response so transport and body failures share one retry
// budget. Network bodies are closed before retrying; the returned in-memory body
// belongs to the caller. A zero limit preserves existing unbounded readers.
func DoRead(request *http.Request, attemptTimeout time.Duration, maxBytes int64, do func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return nil, fmt.Errorf("read request body is not replayable")
	}
	if request.Body != nil {
		defer func() { _ = request.Body.Close() }()
	}
	var response *http.Response
	err := RetryRead(request.Context(), attemptTimeout, func(ctx context.Context) (int, http.Header, error) {
		attempt := request.Clone(ctx)
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return 0, nil, err
			}
			attempt.Body = body
		}
		resp, err := do(attempt)
		response = resp
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return 0, nil, err
		}
		if resp == nil || resp.Body == nil {
			return 0, nil, errInvalidReadResponse
		}
		var reader io.Reader = resp.Body
		if maxBytes > 0 {
			reader = io.LimitReader(reader, maxBytes+1)
		}
		body, readErr := io.ReadAll(reader)
		_ = resp.Body.Close()
		if maxBytes > 0 && int64(len(body)) > maxBytes {
			readErr = fmt.Errorf("%w: limit %d bytes", ErrReadResponseTooLarge, maxBytes)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp.StatusCode, resp.Header, readErr
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}
