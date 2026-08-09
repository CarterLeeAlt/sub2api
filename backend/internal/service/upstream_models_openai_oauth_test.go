package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestExtractOpenAICodexManifestModelIDs(t *testing.T) {
	t.Parallel()

	models, err := extractOpenAICodexManifestModelIDs([]byte(`{"models":[{"slug":"gpt-5.5"},{"slug":"gpt-5.4"},{"slug":"gpt-5.5"},{"slug":""}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5"}, models)

	_, err = extractOpenAICodexManifestModelIDs([]byte(`not-json`))
	require.Error(t, err)
}

func TestFetchUpstreamSupportedModelsSupportsOpenAIOAuth(t *testing.T) {
	var gotAuthorization string
	var gotAccountID string
	var gotOriginator string
	var gotVersion string
	var gotClientVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		gotOriginator = r.Header.Get("Originator")
		gotVersion = r.Header.Get("Version")
		gotClientVersion = r.URL.Query().Get("client_version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5"},{"slug":"gpt-5.4"},{"slug":"gpt-5.5"}]}`))
	}))
	defer server.Close()

	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = originalURL }()

	svc := &AccountTestService{}
	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-access-token",
			"chatgpt_account_id": "acc-123",
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5"}, models)
	require.Equal(t, "Bearer oauth-access-token", gotAuthorization)
	require.Equal(t, "acc-123", gotAccountID)
	require.Equal(t, openai.CodexDefaultOriginator, gotOriginator)
	require.Equal(t, codexCLIVersion, gotVersion)
	require.Equal(t, codexCLIVersion, gotClientVersion)
}

func TestFetchUpstreamSupportedModelsOpenAIOAuthRequiresAccessToken(t *testing.T) {
	t.Parallel()

	svc := &AccountTestService{}
	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:          8,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	})
	require.Error(t, err)

	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr))
	require.Equal(t, UpstreamModelSyncErrorConfiguration, syncErr.Kind)
	require.Equal(t, "No OpenAI access token is available", syncErr.SafeMessage())
}
