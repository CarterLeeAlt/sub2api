package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExtractOpenAICodexManifestModelIDs(t *testing.T) {
	t.Parallel()

	models, err := extractOpenAICodexManifestModelIDs([]byte(`{"models":[{"slug":"gpt-5.5","supported_in_api":true},{"slug":"gpt-5.4"},{"slug":"gpt-5.5","supported_in_api":true},{"slug":"gpt-hidden","supported_in_api":false},{"slug":""}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5"}, models)

	_, err = extractOpenAICodexManifestModelIDs([]byte(`not-json`))
	require.Error(t, err)
}

func TestSelectOpenAICodexImageMainModel(t *testing.T) {
	t.Parallel()

	supported := true
	unsupported := false
	priority1 := 1
	priority2 := 2
	priority9 := 9

	t.Run("keeps legacy preference when available", func(t *testing.T) {
		models := []openAICodexManifestModel{
			{Slug: "gpt-5.6-sol", SupportedInAPI: &supported, Priority: &priority1, InputModalities: []string{"text", "image"}},
			{Slug: openAIImagesResponsesMainModel, SupportedInAPI: &supported, Priority: &priority9, InputModalities: []string{"text", "image"}},
		}
		require.Equal(t, openAIImagesResponsesMainModel, selectOpenAICodexImageMainModel(models))
	})

	t.Run("falls back to highest priority image capable model", func(t *testing.T) {
		models := []openAICodexManifestModel{
			{Slug: "gpt-5.5", SupportedInAPI: &supported, Priority: &priority2, InputModalities: []string{"text", "image"}},
			{Slug: "gpt-5.6-sol", SupportedInAPI: &supported, Priority: &priority1, InputModalities: []string{"text", "image"}},
			{Slug: "gpt-5.6-text-only", SupportedInAPI: &supported, Priority: &priority1, InputModalities: []string{"text"}},
			{Slug: "gpt-hidden", SupportedInAPI: &unsupported, Priority: &priority1, InputModalities: []string{"text", "image"}},
		}
		require.Equal(t, "gpt-5.6-sol", selectOpenAICodexImageMainModel(models))
	})

	t.Run("omitted modalities use Codex legacy image capable default", func(t *testing.T) {
		models := []openAICodexManifestModel{{
			Slug:           "gpt-legacy",
			SupportedInAPI: &supported,
			Priority:       &priority1,
		}}
		require.Equal(t, "gpt-legacy", selectOpenAICodexImageMainModel(models))
	})

	t.Run("image tool models are not selected as Responses orchestrators", func(t *testing.T) {
		models := []openAICodexManifestModel{{
			Slug:            "gpt-image-2",
			SupportedInAPI:  &supported,
			Priority:        &priority1,
			InputModalities: []string{"text", "image"},
		}}
		require.Empty(t, selectOpenAICodexImageMainModel(models))
	})
}

func TestOpenAICodexImageGenerationEligible(t *testing.T) {
	t.Parallel()

	supported := true
	unsupported := false
	priority1 := 1
	imageCapableModel := openAICodexManifestModel{
		Slug:            "gpt-5.6-sol",
		SupportedInAPI:  &supported,
		Priority:        &priority1,
		InputModalities: []string{"text", "image"},
	}

	tests := []struct {
		name   string
		plan   string
		models []openAICodexManifestModel
		want   bool
	}{
		{
			name:   "paid account with any image capable Codex model",
			plan:   "plus",
			models: []openAICodexManifestModel{imageCapableModel},
			want:   true,
		},
		{
			name:   "free account is not eligible",
			plan:   "free",
			models: []openAICodexManifestModel{imageCapableModel},
			want:   false,
		},
		{
			name:   "unknown plan is conservative",
			plan:   "",
			models: []openAICodexManifestModel{imageCapableModel},
			want:   false,
		},
		{
			name: "text only catalog is not eligible",
			plan: "pro",
			models: []openAICodexManifestModel{{
				Slug:            "gpt-5.6-sol",
				SupportedInAPI:  &supported,
				Priority:        &priority1,
				InputModalities: []string{"text"},
			}},
			want: false,
		},
		{
			name: "unsupported image capable model is not eligible",
			plan: "business",
			models: []openAICodexManifestModel{{
				Slug:            "gpt-5.6-sol",
				SupportedInAPI:  &unsupported,
				Priority:        &priority1,
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

func TestBuildOpenAIImagesResponsesRequestForMainModel(t *testing.T) {
	t.Parallel()

	body, err := buildOpenAIImagesResponsesRequestForMainModel(&OpenAIImagesRequest{
		Endpoint: openAIImagesGenerationsEndpoint,
		Prompt:   "draw a cat",
		N:        1,
	}, "gpt-5.6-sol", "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(body, "model").String())
	require.Equal(t, "image_generation", gjson.GetBytes(body, "tools.0.type").String())
	require.Equal(t, "gpt-image-2", gjson.GetBytes(body, "tools.0.model").String())
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

func TestFetchUpstreamSupportedModelsOpenAIOAuthAddsImageModelWithoutLegacyMainModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","supported_in_api":true,"priority":1,"input_modalities":["text","image"]},{"slug":"gpt-5.5","supported_in_api":true,"priority":2,"input_modalities":["text"]},{"slug":"gpt-image-2"},{"slug":"gpt-image-2"}]}`))
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
	require.Equal(t, []string{"gpt-5.5", "gpt-5.6-sol", "gpt-image-2"}, models)
}

func TestFetchUpstreamSupportedModelsOpenAIOAuthDoesNotAddImageModelForFreePlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","supported_in_api":true,"priority":1,"input_modalities":["text","image"]},{"slug":"gpt-5.5","supported_in_api":true}]}`))
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
	require.Equal(t, []string{"gpt-5.5", "gpt-5.6-sol"}, models)
}

func TestResolveOpenAIImagesResponsesMainModelUsesManifestPriority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","supported_in_api":true,"priority":2,"input_modalities":["text","image"]},{"slug":"gpt-5.6-sol","supported_in_api":true,"priority":1,"input_modalities":["text","image"]}]}`))
	}))
	defer server.Close()

	originalURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = originalURL }()

	svc := &OpenAIGatewayService{}
	model, err := svc.resolveOpenAIImagesResponsesMainModel(context.Background(), &Account{
		ID:       117,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-access-token",
			"chatgpt_account_id": "acc-117",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", model)
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
