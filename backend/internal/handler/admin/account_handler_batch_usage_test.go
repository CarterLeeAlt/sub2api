package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type batchUsageAccountRepo struct {
	service.AccountRepository
	cancel context.CancelFunc
	err    error
}

func (r *batchUsageAccountRepo) GetByIDs(context.Context, []int64) ([]*service.Account, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return []*service.Account{{ID: 123, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth}}, r.err
}

type batchUsageAuditRepo struct {
	service.AuditLogRepository
	entries []*service.AuditLog
}

func (r *batchUsageAuditRepo) BatchInsert(_ context.Context, entries []*service.AuditLog) (int64, error) {
	r.entries = append(r.entries, entries...)
	return int64(len(entries)), nil
}

func TestAccountHandlerBatchUsageReadStatus(t *testing.T) {
	const path = "/api/v1/admin/accounts/usage/batch"
	for _, tc := range []struct {
		name          string
		cancelRequest bool
		readErr       error
		wantStatus    int
	}{
		{name: "account query succeeds", wantStatus: http.StatusOK},
		{name: "cancel during account query", cancelRequest: true, readErr: context.Canceled, wantStatus: httputil.StatusClientClosedRequest},
		{name: "driver error after disconnect", cancelRequest: true, readErr: errors.New("query canceled by driver"), wantStatus: httputil.StatusClientClosedRequest},
		{name: "cancel with a batch result", cancelRequest: true, wantStatus: httputil.StatusClientClosedRequest},
		{name: "database error", readErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "query canceled with active request", readErr: context.Canceled, wantStatus: http.StatusInternalServerError},
		{name: "query timed out with active request", readErr: context.DeadlineExceeded, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			repo := &batchUsageAccountRepo{err: tc.readErr}
			if tc.cancelRequest {
				repo.cancel = cancel
			}
			usage := service.NewAccountUsageService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			handler := &AccountHandler{accountUsageService: usage}
			auditRepo := &batchUsageAuditRepo{}
			audit := service.NewAuditLogService(auditRepo, nil)
			audit.Start()
			t.Cleanup(audit.Stop)
			router := gin.New()
			router.POST(path, gin.HandlerFunc(servermiddleware.NewAuditLogMiddleware(audit)), handler.GetBatchUsage)
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(`{"account_ids":[123,456]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			audit.Stop()

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			require.Len(t, auditRepo.entries, 1)
			require.Equal(t, path, auditRepo.entries[0].Path)
			require.Equal(t, tc.wantStatus, auditRepo.entries[0].StatusCode)
			switch tc.wantStatus {
			case httputil.StatusClientClosedRequest:
				require.Empty(t, rec.Body.String())
			case http.StatusInternalServerError:
				require.Contains(t, rec.Body.String(), "internal error")
				require.NotContains(t, rec.Body.String(), tc.readErr.Error())
			case http.StatusOK:
				require.Contains(t, rec.Body.String(), `"usage":{"123":`)
				require.Contains(t, rec.Body.String(), `"errors":{"456":`)
			}
		})
	}
}
