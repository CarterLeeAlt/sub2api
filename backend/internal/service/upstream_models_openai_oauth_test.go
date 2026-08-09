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

	models, err := extractOpenAICodexManifestModelIDs([]byte(`{"models":[{"slug":"gpt-5.5","supported_in_api":true},{"slug":"gpt-5.4"},{"slug":"gpt-5.5","supported_in_api":true},{"slug":"gpt-hidden","supported_in_api":false},{"slug":""}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5"}, models)

	_, err = extractOpenAICodexManifestModelIDs([]byte(`not-json`))
	require.Error(t, err)
}

func TestOpenAICodexImageGenerationEligible(t *testing.T) {
	t.Parallel()

	supported := true
	unsupported := false
	imageCapableMainModel := openAICodexManifestModel{
		Slug:            openAIImagesResponsesMainModel,
		SupportedInAPI:  &supported,
		InputModalities: []string{"text", "image"},
	}

	tests := []struct {
		name   string
		plan   string
		models []openAICodexManifestModel
		want   bool
	}{
		{
			name:   "paid account with image capable main model",
			plan:   "plus",
			models: []openAICodexManifestModel{imageCapableMainModel},
			want:   true,
		},
		{
			name:   "free account is not eligible",
			plan:   "free",
			models: []openAICodexManifestModel{imageCapableMainModel},
			want:   false,
		},
		{
			name:   "unknown plan is conservative",
			plan:   "",
			models: []openAICodexManifestModel{imageCapableMainModel},
			want:   false,
		},
		{
			name: "main model without image input is not eligible",
			plan: "pro",
			models: []openAICodexManifestModel{{
				Slug:            openAIImagesResponsesMainModel,
				SupportedInAPI:  &supported,
				InputModalities: []string{"text"},
			}},
			want: false,
		},
		{
			name: "unsupported main model is not eligible",
			plan: "business",
			models: []openAICodexManifestModel{{
				Slug:            openAIImagesResponsesMainModel,
				SupportedInAPI:  &unsupported,
				InputModalities: []string{"text", "image"},
			}},
			want: false,
		},
		{
			name: "another image capable model does not replace bridge main model",
			plan: "team",
			models: []openAICodexManifestModel{{
				Slug:            "gpt-5.6-sol",
				SupportedInAPI:  &supported,
				InputModalities: []string{"text", "image"},
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"plan_type": tt.plan,
				},
			}
			require.Equal(t, tt.want, openAICodexImageGenerationEligible(account, tt.models))
		})
	}
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
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","supported_in_api":true},{"slug":"gpt-5.4","supported_in_api":true},{"slug":"gpt-5.5","supported_in_api":true}]}`))
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

func TestFetchUpstreamSupportedModelsOpenAIOAuthAddsImageModelWhenEligible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","supported_in_api":true,"input_modalities":["text","image"]},{"slug":"gpt-5.4-mini","supported_in_api":true,"input_modalities":["text","image"]},{"slug":"gpt-image-2"},{"slug":"gpt-image-2"}]}`))
	}))
	defer server.Close()

	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = originalURL }()

	svc := &AccountTestService{}
	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       9,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-access-token",
			"chatgpt_account_id": "acc-123",
			"plan_type":          "plus",
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4-mini", "gpt-5.5", "gpt-image-2"}, models)
}

func TestFetchUpstreamSupportedModelsOpenAIOAuthDoesNotAddImageModelForFreePlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.4-mini","supported_in_api":true,"input_modalities":["text","image"]},{"slug":"gpt-5.5","supported_in_api":true}]}`))
	}))
	defer server.Close()

	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = originalURL }()

	svc := &AccountTestService{}
	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       10,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-access-token",
			"chatgpt_account_id": "acc-123",
			"plan_type":          "free",
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4-mini", "gpt-5.5"}, models)
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
