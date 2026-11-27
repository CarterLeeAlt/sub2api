package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

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
// Both transports persist credential-scoped diagnostics through the same CAS
// writer. The request snapshot prevents late failures from disabling new credentials.
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
			errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if readErr != nil {
				return nil, nil, newUpstreamModelSyncUpstreamError("Failed to read upstream error", readErr)
			}
			if credentialAccount.IsOpenAIAgentIdentity() && !agentIdentityTaskRecoveryWasTried(ctx) && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, errorBody) {
				_ = resp.Body.Close()
				if err := ensureAgentIdentityTaskForAccount(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount, credentialAccount.GetCredential("task_id")); err != nil {
					return nil, nil, newUpstreamModelSyncUpstreamError("Failed to renew Agent Identity task", err)
				}
				return s.fetchOpenAIOAuthUpstreamModels(markAgentIdentityTaskRecoveryTried(ctx), account)
			}
			_ = observeOpenAIUpstreamResponse(ctx, s.accountRepo, credentialAccount, "models", resp.StatusCode, errorBody)
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
		_ = observeOpenAIUpstreamResponse(ctx, s.accountRepo, credentialAccount, "models", http.StatusOK, nil)
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
		// Transport errors are classified here with the request's credential owner.
		gateway := &OpenAIGatewayService{accountRepo: s.accountRepo}
		var manifest *OpenAIModelsResponse
		err := httpclient.RetryRead(ctx, 15*time.Second, func(attemptCtx context.Context) (int, http.Header, error) {
			var fetchErr error
			manifest, fetchErr = gateway.FetchCodexModelsManifest(attemptCtx, account, "", "")
			if fetchErr == nil {
				return http.StatusOK, nil, nil
			}
			var upstreamErr *codexModelsManifestUpstreamError
			if errors.As(fetchErr, &upstreamErr) && upstreamErr.retryable {
				return upstreamErr.statusCode, upstreamErr.headers, fetchErr
			}
			return http.StatusBadRequest, nil, fetchErr
		})
		if err != nil {
			var upstreamErr *codexModelsManifestUpstreamError
			if errors.As(err, &upstreamErr) {
				_ = observeOpenAIUpstreamResponse(ctx, s.accountRepo, credentialAccount, "models", upstreamErr.statusCode, upstreamErr.body)
			}
			return nil, nil, newUpstreamModelSyncUpstreamError("Failed to fetch OpenAI Codex model list", err)
		}
		if manifest == nil || len(manifest.Body) == 0 {
			return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
		}
		body = manifest.Body
		_ = observeOpenAIUpstreamResponse(ctx, s.accountRepo, credentialAccount, "models", http.StatusOK, nil)
	}

	if len(body) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	manifestModels, err := parseOpenAICodexManifestModels(body)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("OpenAI Codex model list response was not valid JSON", err)
	}
	models := openAICodexManifestModelIDs(manifestModels)
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	spec := officialImageModels.Load()
	canGenerate := openAICodexImageGenerationEligible(credentialAccount, manifestModels)
	if spec == nil || spec.Lifecycle == nil {
		var officialModels map[string]imageEndpointSupport
		if spec != nil {
			officialModels = spec.Models
		}
		textModels := slices.DeleteFunc(models, func(id string) bool { return isOfficialImageModelID(id, officialModels) })
		// Keep a saved automatic whitelist intact while the shared catalog loads.
		if len(textModels) == 0 || (canGenerate && account.ModelCatalog().Ready) {
			return nil, nil, newUpstreamModelSyncUpstreamError("OpenAI image catalog is not available yet", nil)
		}
		return textModels, body, nil
	}
	if canGenerate {
		excluded := make(map[string]bool)
		for _, entry := range manifestModels {
			if entry.SupportedInAPI != nil && !*entry.SupportedInAPI {
				excluded[strings.TrimSpace(entry.Slug)] = true
			}
		}
		for id, endpoints := range spec.Models {
			if endpoints.Generate && !excluded[id] {
				models = append(models, id)
			}
		}
	}
	models = spec.Lifecycle.filterModelIDs(models)
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	return models, body, nil
}

func openAICodexImageGenerationEligible(account *Account, manifestModels []openAICodexManifestModel) bool {
	if account == nil || !account.IsOpenAIOAuth() {
		return false
	}
	plan := openAICodexPlanType(account)
	return plan != "" && !strings.EqualFold(plan, "free") && selectOpenAICodexImageMainModel(manifestModels) != ""
}

func openAICodexPlanType(account *Account) string {
	if plan := strings.TrimSpace(account.GetCredential("plan_type")); plan != "" {
		return plan
	}
	for _, key := range []string{"id_token", "access_token"} {
		claims, err := openai.DecodeIDToken(account.GetCredential(key))
		if err == nil && claims.OpenAIAuth != nil && strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType) != "" {
			return strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType)
		}
	}
	return ""
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
