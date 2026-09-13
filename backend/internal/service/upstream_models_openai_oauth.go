package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
//
// When the sync service has a dedicated upstream transport it is used so the
// request stays observable and injectable (tests, TLS fingerprinting, proxy
// handling parity with the other platforms); otherwise the gateway's manifest
// client remains the transport of record.
//
// Known limitation of the fallback transport: the half-initialized gateway
// instance below only carries accountRepo. Account-auth failures (e.g. 401)
// that reach handleCodexModelsManifestAccountAuthError →
// handleOpenAIAccountUpstreamError deliberately skip every account-state side
// effect because those are gated on rateLimitService != nil. This admin sync
// path therefore never trips account circuit breakers; that responsibility
// stays with the main request path, which runs on a fully initialized gateway
// (httpUpstream configured, rateLimitService wired).
func (s *AccountTestService) fetchOpenAIOAuthUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return nil, nil, newUpstreamModelSyncConfigError("Failed to resolve OpenAI OAuth credentials", err)
	}
	if credentialAccount == nil || !credentialAccount.IsOpenAIOAuth() {
		return nil, nil, newUpstreamModelSyncUnsupportedError("OpenAI OAuth credentials are required for Codex model sync", nil)
	}
	if !credentialAccount.IsOpenAIAgentIdentity() && strings.TrimSpace(credentialAccount.GetOpenAIAccessToken()) == "" {
		return nil, nil, newUpstreamModelSyncConfigError("No OpenAI access token is available", nil)
	}

	var body []byte
	if s.httpUpstream != nil {
		req, err := s.buildOpenAIOAuthUpstreamModelsRequest(ctx, account)
		if err != nil {
			return nil, nil, err
		}
		resp, err := s.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
		if err != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to request upstream model list", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, nil, &UpstreamModelSyncError{
				Kind:       UpstreamModelSyncErrorUpstream,
				Message:    fmt.Sprintf("Upstream model list request failed with HTTP %d", resp.StatusCode),
				StatusCode: resp.StatusCode,
				Err:        fmt.Errorf("upstream model list returned HTTP %d", resp.StatusCode),
			}
		}
		bodyLimit := resolveModelsListReadLimit(s.cfg)
		body, err = io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
		if err != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read upstream model list", err)
		}
		if int64(len(body)) > bodyLimit {
			return nil, nil, newUpstreamModelSyncUpstreamError("Upstream model list response is too large", fmt.Errorf("response exceeds %d bytes", bodyLimit))
		}
	} else {
		// FetchCodexModelsManifest already owns the ChatGPT Codex endpoint, request
		// headers, account-id/FedRAMP handling, proxy behavior, Agent Identity auth,
		// response limits, and manifest-envelope validation. Keep the admin sync path
		// on that implementation instead of duplicating the protocol here.
		//
		// Pass an empty client version so the manifest request resolves the live
		// canonical version (admin override → auto-synced → compiled constant). The
		// Codex backend gates manifest entries by client_version (e.g. gpt-6 models
		// only ship to >= 0.153.0), so pinning the compiled constant would freeze the
		// synced catalog at whatever models shipped with that historical version.
		// Half-initialized fallback transport: only accountRepo is injected.
		// handleCodexModelsManifestAccountAuthError may still fire on a 401,
		// but every account-state side effect inside
		// handleOpenAIAccountUpstreamError is gated on rateLimitService != nil,
		// so this path intentionally performs no account circuit breaking. The
		// main gateway path (httpUpstream configured) owns that duty; injecting
		// a full dependency graph here is deliberately avoided.
		gateway := &OpenAIGatewayService{accountRepo: s.accountRepo}
		manifest, err := gateway.FetchCodexModelsManifest(ctx, account, "", "")
		if err != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to fetch OpenAI Codex model list", err)
		}
		if manifest == nil || len(manifest.Body) == 0 {
			return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
		}
		body = manifest.Body
	}

	if len(body) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	manifestModels, err := parseOpenAICodexManifestModels(body)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("OpenAI Codex model list response was not valid JSON", err)
	}
	models := openAICodexManifestModelIDs(manifestModels)
	if openAICodexImageGenerationEligible(credentialAccount, manifestModels) {
		models = append(models, openAICodexImageGenerationModel)
		models = dedupeAndSortModelIDs(models)
	}
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	return models, body, nil
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
// the image_generation tool. Honor the operator's preferred main model
// (openAIImagesResponsesMainModelValue, env-overridable) while it is actually
// available, but do not make image capability depend on that specific slug.
// Otherwise follow the Codex catalog priority order.
func selectOpenAICodexImageMainModel(manifestModels []openAICodexManifestModel) string {
	preferred := strings.TrimSpace(openAIImagesResponsesMainModelValue())
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
