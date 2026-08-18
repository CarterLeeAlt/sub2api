//go:build unit

package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountHandlerCreateOwnsSingleOpenAIPrivacyAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := &privacyCreateAdminService{stubAdminService: newStubAdminService()}
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts", handler.Create)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewBufferString(
		`{"name":"openai-oauth","platform":"openai","type":"oauth","credentials":{"access_token":"token"}}`,
	))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, adminSvc.createdAccounts, 1)
	require.True(t, adminSvc.createdAccounts[0].SkipAutomaticPrivacySetup)
	require.Equal(t, 1, adminSvc.openAIPrivacyCalls)
	require.Contains(t, recorder.Body.String(), `"privacy_mode":"training_off"`)
}

type privacyCreateAdminService struct {
	*stubAdminService
	openAIPrivacyCalls int
}

func (s *privacyCreateAdminService) CreateAccount(ctx context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	account, err := s.stubAdminService.CreateAccount(ctx, input)
	if err != nil {
		return nil, err
	}
	account.Platform = input.Platform
	account.Type = input.Type
	account.Credentials = input.Credentials
	account.Extra = input.Extra
	return account, nil
}

func (s *privacyCreateAdminService) EnsureOpenAIPrivacy(ctx context.Context, account *service.Account) string {
	s.openAIPrivacyCalls++
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra["privacy_mode"] = service.PrivacyModeTrainingOff
	return service.PrivacyModeTrainingOff
}
