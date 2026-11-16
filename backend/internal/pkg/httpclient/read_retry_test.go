package httpclient

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type brokenReadBody struct{ closed *int }

func (b brokenReadBody) Read(p []byte) (int, error) {
	return copy(p, `{"partial":`), io.ErrUnexpectedEOF
}
func (b brokenReadBody) Close() error { *b.closed++; return nil }

func TestDoReadRecoversBodyFailureAndReplaysReadOnlyPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/query", strings.NewReader(`{"id":1}`))
	require.NoError(t, err)
	closed, calls := 0, 0
	resp, err := DoRead(req, time.Second, 1024, func(r *http.Request) (*http.Response, error) {
		calls++
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		require.Equal(t, `{"id":1}`, string(body))
		_ = r.Body.Close()
		result := io.NopCloser(strings.NewReader(`{"ok":true}`))
		if calls == 1 {
			result = brokenReadBody{&closed}
		}
		return &http.Response{StatusCode: 200, Body: result}, nil
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"ok":true}`, string(body))
	require.Equal(t, 2, calls)
	require.Equal(t, 1, closed)
}

func TestReadRetryStatusAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		hint    string
		body    string
		calls   int
		wantErr bool
	}{
		{"success", 200, "", "ok", 1, false}, {"500", 500, "", "", 2, false},
		{"502", 502, "", "", 2, false}, {"503", 503, "", "", 2, false}, {"504", 504, "", "", 2, false},
		{"401", 401, "", "", 1, false}, {"403", 403, "", "", 1, false}, {"404", 404, "", "", 1, false},
		{"429 without hint", 429, "", "", 1, false}, {"429 short hint", 429, "0", "", 2, false},
		{"long hint", 503, "120", "", 1, false}, {"429 invalid hint", 429, "invalid", "", 1, false},
		{"date cooldown", 503, time.Now().Add(time.Minute).UTC().Format(http.TimeFormat), "", 1, false},
		{"408", 408, "", "", 2, false},
		{"oversized", 200, "", "123456789", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
			calls := 0
			resp, err := DoRead(req, time.Second, 8, func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{tc.hint}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrReadResponseTooLarge)
			} else {
				require.NoError(t, err)
				_ = resp.Body.Close()
			}
			require.Equal(t, tc.calls, calls)
		})
	}
}

func TestReadRetryPermanentTransportErrorsAndInvalidResponse(t *testing.T) {
	for _, failure := range []error{context.Canceled, x509.UnknownAuthorityError{}, x509.HostnameError{}, x509.CertificateInvalidError{}} {
		calls := 0
		err := RetryRead(context.Background(), time.Second, func(context.Context) (int, http.Header, error) { calls++; return 0, nil, failure })
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, calls)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	calls := 0
	_, err := DoRead(req, time.Second, 100, func(*http.Request) (*http.Response, error) { calls++; return nil, nil })
	require.ErrorIs(t, err, errInvalidReadResponse)
	require.Equal(t, 1, calls)
}

func TestReadRetryIndependentDeadlineAndCancellation(t *testing.T) {
	calls := 0
	err := RetryRead(context.Background(), 10*time.Millisecond, func(ctx context.Context) (int, http.Header, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return 0, nil, ctx.Err()
		}
		require.NoError(t, ctx.Err())
		return 200, nil, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	err = RetryRead(ctx, time.Second, func(context.Context) (int, http.Header, error) { calls++; cancel(); return 0, nil, io.EOF })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	calls = 0
	err = RetryRead(context.Background(), time.Second, func(context.Context) (int, http.Header, error) {
		calls++
		return 0, nil, errors.New("connection reset")
	})
	require.Error(t, err)
	require.Equal(t, 2, calls)
}
