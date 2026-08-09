package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

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

	models, err := extractOpenAICodexManifestModelIDs(manifest.Body)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("OpenAI Codex model list response was not valid JSON", err)
	}
	if len(models) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	return models, nil
}

func extractOpenAICodexManifestModelIDs(body []byte) ([]string, error) {
	var manifest struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("parse OpenAI Codex model manifest: %w", err)
	}

	models := make([]string, 0, len(manifest.Models))
	for _, model := range manifest.Models {
		models = append(models, model.Slug)
	}
	return dedupeAndSortModelIDs(models), nil
}
