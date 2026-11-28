//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/bcrypt"
)

type captchaReadTestSettingRepo struct {
	oauthCaptchaSettingRepo
	readError error
	cancel    context.CancelFunc
}

func (r *captchaReadTestSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if r.cancel != nil {
		r.cancel()
	}
	if r.readError != nil {
		return nil, r.readError
	}
	return r.oauthCaptchaSettingRepo.GetMultiple(ctx, keys)
}

type captchaReadTestUserRepo struct {
	service.UserRepository
	user  *service.User
	reads int
}

func (r *captchaReadTestUserRepo) GetByEmail(context.Context, string) (*service.User, error) {
	r.reads++
	return r.user, nil
}

func (r *captchaReadTestUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

type captchaReadTestAuditRepo struct {
	service.AuditLogRepository
	entries []*service.AuditLog
}

func (r *captchaReadTestAuditRepo) BatchInsert(_ context.Context, entries []*service.AuditLog) (int64, error) {
	r.entries = append(r.entries, entries...)
	return int64(len(entries)), nil
}

func TestLoginCaptchaSettingsRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const requestID = "login-captcha-fixture"
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("valid-password"), bcrypt.MinCost)
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		readError    error
		cancel       bool
		captcha      bool
		proof        string
		password     string
		wantStatus   int
		wantResponse string
		wantCause    string
		wantReads    int
		wantVerify   int
	}{
		{name: "client disconnected", cancel: true, readError: errors.New("pq: canceling statement due to user request"), wantStatus: 499},
		{name: "settings unavailable", readError: errors.New("dial: connection refused password=fixture-secret"), wantStatus: 503, wantResponse: "SERVICE_UNAVAILABLE", wantCause: "connection refused password=***"},
		{name: "database timeout", readError: context.DeadlineExceeded, wantStatus: 503, wantResponse: "SERVICE_UNAVAILABLE", wantCause: "context deadline exceeded"},
		{name: "database canceled while request active", readError: context.Canceled, wantStatus: 503, wantResponse: "SERVICE_UNAVAILABLE", wantCause: "context canceled"},
		{name: "captcha disabled", password: "valid-password", wantStatus: 200, wantResponse: "access_token", wantReads: 1},
		{name: "wrong password", password: "invalid-password", wantStatus: 401, wantResponse: "INVALID_CREDENTIALS", wantReads: 1},
		{name: "captcha required", captcha: true, password: "valid-password", wantStatus: 400, wantResponse: "TENCENT_CAPTCHA_VERIFICATION_FAILED"},
		{name: "captcha verified", captcha: true, proof: `,"tencent_captcha_ticket":"ticket","tencent_captcha_randstr":"@rand"`, password: "valid-password", wantStatus: 200, wantResponse: "access_token", wantReads: 1, wantVerify: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			core, logs := observer.New(zap.ErrorLevel)
			ctx = context.WithValue(ctx, ctxkey.RequestID, requestID)
			ctx = logger.IntoContext(ctx, zap.New(core).With(zap.String("request_id", requestID)))
			settingsRepo := &captchaReadTestSettingRepo{readError: tc.readError}
			if tc.cancel {
				settingsRepo.cancel = cancel
			}
			if tc.captcha {
				settingsRepo.values = map[string]string{
					service.SettingKeyTencentCaptchaEnabled:        "true",
					service.SettingKeyTencentCaptchaAppID:          "123456789",
					service.SettingKeyTencentCaptchaAppSecretKey:   "app-secret",
					service.SettingKeyTencentCaptchaCloudSecretID:  "cloud-secret-id",
					service.SettingKeyTencentCaptchaCloudSecretKey: "cloud-secret-key",
				}
			}
			users := &captchaReadTestUserRepo{user: &service.User{
				ID: 1, Email: "user@example.com", PasswordHash: string(passwordHash), Role: service.RoleAdmin, Status: service.StatusActive,
			}}
			cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-jwt-secret", ExpireHour: 1}}
			settings := service.NewSettingService(settingsRepo, cfg)
			verifier := &oauthCaptchaVerifier{}
			auth := service.NewAuthService(nil, users, nil, &oauthPendingFlowRefreshTokenCacheStub{}, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
			auth.SetTencentCaptchaService(service.NewTencentCaptchaService(settings, verifier))
			handler := NewAuthHandler(cfg, auth, nil, settings, nil, nil, nil, nil)
			auditRepo := &captchaReadTestAuditRepo{}
			audit := service.NewAuditLogService(auditRepo, settings)
			audit.Start()
			t.Cleanup(audit.Stop)
			router := gin.New()
			router.POST("/api/v1/auth/login", gin.HandlerFunc(servermiddleware.NewAuditLogMiddleware(audit)), handler.Login)
			password := tc.password
			if password == "" {
				password = "valid-password"
			}
			body := `{"email":"user@example.com","password":"` + password + `"` + tc.proof + `}`
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			audit.Stop()

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			require.Len(t, auditRepo.entries, 1)
			require.Equal(t, tc.wantStatus, auditRepo.entries[0].StatusCode)
			require.Equal(t, requestID, auditRepo.entries[0].RequestID)
			if tc.wantResponse == "" {
				require.Empty(t, rec.Body.String())
			} else {
				require.Contains(t, rec.Body.String(), tc.wantResponse)
			}
			require.NotContains(t, rec.Body.String(), "fixture-secret")
			require.Equal(t, tc.wantReads, users.reads)
			require.Equal(t, tc.wantVerify, verifier.calls)
			if tc.wantCause == "" {
				require.Zero(t, logs.Len())
				return
			}
			require.NotContains(t, rec.Body.String(), tc.wantCause)
			require.Equal(t, 1, logs.Len())
			fields := logs.All()[0].ContextMap()
			require.Equal(t, requestID, fields["request_id"])
			require.Contains(t, fields["error"], tc.wantCause)
			require.NotContains(t, fields["error"], "fixture-secret")
		})
	}
}

func TestAuthCaptchaRoutesMarkDisconnectedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"email":"unused@example.com","password":"valid-password"}`
	for _, path := range []string{
		"/api/v1/auth/register", "/api/v1/auth/send-verify-code", "/api/v1/auth/forgot-password",
		"/api/v1/auth/oauth/github/start", "/api/v1/auth/passkey/login/begin",
		"/api/v1/auth/oauth/pending/send-verify-code", "/api/v1/auth/oauth/oidc/create-account",
	} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture, client := newOAuthPendingFlowTestHandler(t, false)
			cfg := &config.Config{}
			cfg.WebAuthn.Enabled = true
			settings := service.NewSettingService(&captchaReadTestSettingRepo{readError: context.Canceled, cancel: cancel}, cfg)
			auth := service.NewAuthService(client, nil, nil, nil, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
			fixture.authService, fixture.settingSvc = auth, settings
			passkeys := NewPasskeyHandler(nil, auth, settings)
			router := gin.New()
			router.POST("/api/v1/auth/register", fixture.Register)
			router.POST("/api/v1/auth/send-verify-code", fixture.SendVerifyCode)
			router.POST("/api/v1/auth/forgot-password", fixture.ForgotPassword)
			router.POST("/api/v1/auth/oauth/github/start", fixture.GitHubOAuthStart)
			router.POST("/api/v1/auth/passkey/login/begin", passkeys.BeginLogin)
			router.POST("/api/v1/auth/oauth/pending/send-verify-code", fixture.SendPendingOAuthVerifyCode)
			router.POST("/api/v1/auth/oauth/oidc/create-account", fixture.CreateOIDCOAuthAccount)
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if path == "/api/v1/auth/oauth/oidc/create-account" {
				session, err := client.PendingAuthSession.Create().
					SetSessionToken("captcha-read-session").
					SetIntent("login").
					SetProviderType("oidc").
					SetProviderKey("https://issuer.example").
					SetProviderSubject("captcha-read-subject").
					SetBrowserSessionKey("captcha-read-browser").
					SetExpiresAt(time.Now().Add(time.Minute)).
					Save(ctx)
				require.NoError(t, err)
				req.AddCookie(&http.Cookie{Name: oauthPendingSessionCookieName, Value: encodeCookieValue(session.SessionToken)})
				req.AddCookie(&http.Cookie{Name: oauthPendingBrowserCookieName, Value: encodeCookieValue(session.BrowserSessionKey)})
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, 499, rec.Code, rec.Body.String())
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestAuthRequestsBindTencentCaptchaProof(t *testing.T) {
	const payload = `{"email":"user@example.com","password":"secret-123","tencent_captcha_ticket":"ticket-value","tencent_captcha_randstr":"@rand-value"}`

	tests := []struct {
		name   string
		decode func([]byte) service.CaptchaProof
	}{
		{
			name: "登录",
			decode: func(raw []byte) service.CaptchaProof {
				var req LoginRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "注册",
			decode: func(raw []byte) service.CaptchaProof {
				var req RegisterRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "发送邮箱验证码",
			decode: func(raw []byte) service.CaptchaProof {
				var req SendVerifyCodeRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "忘记密码",
			decode: func(raw []byte) service.CaptchaProof {
				var req ForgotPasswordRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "OAuth启动",
			decode: func(raw []byte) service.CaptchaProof {
				var req oauthStartCaptchaRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "Passkey登录",
			decode: func(raw []byte) service.CaptchaProof {
				var req passkeyBeginLoginRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "OAuth待处理账号发送邮箱验证码",
			decode: func(raw []byte) service.CaptchaProof {
				var req sendPendingOAuthVerifyCodeRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
		{
			name: "OAuth待处理账号创建",
			decode: func(raw []byte) service.CaptchaProof {
				var req createPendingOAuthAccountRequest
				require.NoError(t, json.Unmarshal(raw, &req))
				return captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proof := test.decode([]byte(payload))
			require.Equal(t, "ticket-value", proof.TencentTicket)
			require.Equal(t, "@rand-value", proof.TencentRandstr)
		})
	}
}
