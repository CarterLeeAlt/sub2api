package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const openAICodexImageGenerationModel = "gpt-image-2"

type openAICodexManifestModel struct {
	Slug            string   `json:"slug"`
	SupportedInAPI  *bool    `json:"supported_in_api"`
	Priority        *int     `json:"priority"`
	InputModalities []string `json:"input_modalities"`
}

// fetchOpenAIOAuthUpstreamModels reuses the Codex models-manifest path used by
// OpenAI OAuth traffic. OAuth accounts do not expose the public /v1/models
// endpoint used by API-key accounts; their live catalog comes from the ChatGPT
// Codex backend instead.
func (s *AccountTestService) fetchOpenAIOAuthUpstreamModels(ctx context.Context, account *Account) ([]string, error) {
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Failed to resolve OpenAI OAuth credentials", err)
	}
	if credentialAccount == nil || !credentialAccount.IsOpenAIOAuth() {
		return nil, newUpstreamModelSyncUnsupportedError("OpenAI OAuth credentials are required for Codex model sync", nil)
	}
	if !credentialAccount.IsOpenAIAgentIdentity() && strings.TrimSpace(credentialAccount.GetOpenAIAccessToken()) == "" {
		return nil, newUpstreamModelSyncConfigError("No OpenAI access token is available", nil)
	}

	// FetchCodexModelsManifest already owns the ChatGPT Codex endpoint, request
	// headers, account-id/FedRAMP handling, proxy behavior, Agent Identity auth,
	// response limits, and manifest-envelope validation. Keep the admin sync path
	// on that implementation instead of duplicating the protocol here.
	gateway := &OpenAIGatewayService{accountRepo: s.accountRepo}
	manifest, err := gateway.FetchCodexModelsManifest(ctx, account, codexCLIVersion, "")
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to fetch OpenAI Codex model list", err)
	}
	if manifest == nil || len(manifest.Body) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}

	manifestModels, err := parseOpenAICodexManifestModels(manifest.Body)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("OpenAI Codex model list response was not valid JSON", err)
	}
	models := openAICodexManifestModelIDs(manifestModels)
	if openAICodexImageGenerationEligible(credentialAccount, manifestModels) {
		models = append(models, openAICodexImageGenerationModel)
		models = dedupeAndSortModelIDs(models)
	}
	if len(models) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	return models, nil
}

func parseOpenAICodexManifestModels(body []byte) ([]openAICodexManifestModel, error) {
	var manifest struct {
		Models []openAICodexManifestModel `json:"models"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("parse OpenAI Codex model manifest: %w", err)
	}
	return manifest.Models, nil
}

func extractOpenAICodexManifestModelIDs(body []byte) ([]string, error) {
	models, err := parseOpenAICodexManifestModels(body)
	if err != nil {
		return nil, err
	}
	return openAICodexManifestModelIDs(models), nil
}

func openAICodexManifestModelIDs(manifestModels []openAICodexManifestModel) []string {
	models := make([]string, 0, len(manifestModels))
	for _, model := range manifestModels {
		if model.SupportedInAPI != nil && !*model.SupportedInAPI {
			continue
		}
		models = append(models, model.Slug)
	}
	return dedupeAndSortModelIDs(models)
}

func openAICodexManifestModelSupportsImage(model openAICodexManifestModel) bool {
	if model.SupportedInAPI != nil && !*model.SupportedInAPI {
		return false
	}
	slug := strings.TrimSpace(model.Slug)
	if slug == "" {
		return false
	}
	lowerSlug := strings.ToLower(slug)
	if strings.HasPrefix(lowerSlug, "gpt-image-") || lowerSlug == "chatgpt-image-latest" {
		return false
	}

	// Codex treats an omitted input_modalities field as the legacy default of
	// text + image. Mirror that behavior so older manifests are not falsely
	// classified as text-only.
	if len(model.InputModalities) == 0 {
		return true
	}
	for _, modality := range model.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// selectOpenAICodexImageMainModel chooses the Responses model that orchestrates
// the image_generation tool. Keep the historical gpt-5.4-mini preference while
// it is actually available, but do not make image capability depend on that
// specific slug. Otherwise follow the Codex catalog priority order.
func selectOpenAICodexImageMainModel(manifestModels []openAICodexManifestModel) string {
	preferred := strings.TrimSpace(openAIImagesResponsesMainModel)
	var (
		selected            string
		selectedPriority    int
		selectedHasPriority bool
	)

	for _, model := range manifestModels {
		if !openAICodexManifestModelSupportsImage(model) {
			continue
		}
		slug := strings.TrimSpace(model.Slug)
		if preferred != "" && strings.EqualFold(slug, preferred) {
			return slug
		}
		if selected == "" {
			selected = slug
			if model.Priority != nil {
				selectedPriority = *model.Priority
				selectedHasPriority = true
			}
			continue
		}
		if model.Priority == nil {
			continue
		}
		if !selectedHasPriority || *model.Priority < selectedPriority ||
			(*model.Priority == selectedPriority && strings.ToLower(slug) < strings.ToLower(selected)) {
			selected = slug
			selectedPriority = *model.Priority
			selectedHasPriority = true
		}
	}
	return selected
}

// openAICodexImageGenerationEligible mirrors the account/model gates used by
// the official Codex client for exposing image generation, while remaining
// conservative when the account plan cannot be determined. The provider/auth
// gates are already established by a successful authenticated Codex manifest
// request on an OpenAI OAuth account.
func openAICodexImageGenerationEligible(account *Account, manifestModels []openAICodexManifestModel) bool {
	if account == nil || !account.IsOpenAIOAuth() {
		return false
	}

	planType := openAICodexPlanType(account)
	if planType == "" || strings.EqualFold(planType, "free") {
		return false
	}
	return selectOpenAICodexImageMainModel(manifestModels) != ""
}

func openAICodexPlanType(account *Account) string {
	if account == nil {
		return ""
	}
	if planType := strings.TrimSpace(account.GetCredential("plan_type")); planType != "" {
		return strings.ToLower(planType)
	}

	// Older/imported accounts may have the canonical plan claim in a JWT but no
	// persisted plan_type field. Decode only as a best-effort metadata fallback;
	// this is not used to authenticate the request.
	for _, token := range []string{
		account.GetCredential("id_token"),
		account.GetCredential("access_token"),
	} {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		claims, err := openai.DecodeIDToken(token)
		if err != nil || claims.OpenAIAuth == nil {
			continue
		}
		if planType := strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType); planType != "" {
			return strings.ToLower(planType)
		}
	}
	return ""
}
