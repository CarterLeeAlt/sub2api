package admin

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type settingsReadRepo struct {
	settingHandlerRepoStub
	read func(string) error
}

func (r *settingsReadRepo) GetAll(ctx context.Context) (map[string]string, error) {
	if err := r.read("all settings"); err != nil {
		return nil, err
	}
	return r.settingHandlerRepoStub.GetAll(ctx)
}

func (r *settingsReadRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if err := r.read("auth source defaults"); err != nil {
		return nil, err
	}
	return r.settingHandlerRepoStub.GetMultiple(ctx, keys)
}

func (r *settingsReadRepo) GetValue(ctx context.Context, key string) (string, error) {
	if err := r.read(key); err != nil {
		return "", err
	}
	return r.settingHandlerRepoStub.GetValue(ctx, key)
}

func TestSettingHandlerReadStatus(t *testing.T) {
	const path = "/api/v1/admin/settings"
	const fastPolicy = service.SettingKeyOpenAIFastPolicySettings
	const quotas = service.SettingKeyDefaultPlatformQuotas
	for _, tc := range []struct {
		name          string
		read          string
		cancelRequest bool
		readErr       error
		wantStatus    int
		wantFastError bool
	}{
		{name: "normal response", wantStatus: http.StatusOK},
		{name: "cancel during all settings read", read: "all settings", cancelRequest: true, readErr: context.Canceled, wantStatus: httputil.StatusClientClosedRequest},
		{name: "cancel during auth defaults read", read: "auth source defaults", cancelRequest: true, readErr: context.Canceled, wantStatus: httputil.StatusClientClosedRequest},
		{name: "cancel during fast policy read", read: fastPolicy, cancelRequest: true, readErr: context.Canceled, wantStatus: httputil.StatusClientClosedRequest},
		{name: "driver error after disconnect", read: fastPolicy, cancelRequest: true, readErr: errors.New("query canceled by driver"), wantStatus: httputil.StatusClientClosedRequest},
		{name: "cancel during quotas read", read: quotas, cancelRequest: true, readErr: context.Canceled, wantStatus: httputil.StatusClientClosedRequest},
		{name: "all settings read fails", read: "all settings", readErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "auth defaults read fails", read: "auth source defaults", readErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "fast policy read fails", read: fastPolicy, readErr: errors.New("database unavailable"), wantStatus: http.StatusOK, wantFastError: true},
		{name: "fast policy query canceled with active request", read: fastPolicy, readErr: context.Canceled, wantStatus: http.StatusOK, wantFastError: true},
		{name: "fast policy query timed out with active request", read: fastPolicy, readErr: context.DeadlineExceeded, wantStatus: http.StatusOK, wantFastError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousLogger := slog.Default()
			previousWriter, previousFlags := log.Writer(), log.Flags()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() {
				slog.SetDefault(previousLogger)
				log.SetOutput(previousWriter)
				log.SetFlags(previousFlags)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var readsAfterCancel []string
			repo := &settingsReadRepo{
				settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{
					fastPolicy: `{"rules":[{"service_tier":"priority","action":"pass","scope":"all"}]}`,
				}},
				read: func(read string) error {
					if ctx.Err() != nil {
						readsAfterCancel = append(readsAfterCancel, read)
					}
					if read != tc.read {
						return nil
					}
					if tc.cancelRequest {
						cancel()
					}
					return tc.readErr
				},
			}
			svc := service.NewSettingService(repo, &config.Config{})
			handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.GET(path, handler.GetSettings)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			require.Empty(t, readsAfterCancel)
			if tc.wantFastError {
				require.Contains(t, logs.String(), "openai_fast_policy_settings_get_failed")
				require.Contains(t, logs.String(), tc.readErr.Error())
			} else {
				require.NotContains(t, logs.String(), "openai_fast_policy_settings_get_failed")
			}
			if tc.cancelRequest {
				require.Empty(t, rec.Body.String())
				require.Empty(t, logs.String())
			}
			if tc.wantStatus == http.StatusOK {
				data := gjson.GetBytes(rec.Body.Bytes(), "data")
				require.True(t, data.Get("default_platform_quotas").IsObject())
				if tc.wantFastError {
					require.False(t, data.Get("openai_fast_policy_settings").Exists())
				} else {
					require.Equal(t, "priority", data.Get("openai_fast_policy_settings.rules.0.service_tier").String())
				}
			}
		})
	}
}
