package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/sjson"
)

// resolveOpenAIImagesResponsesMainModel selects the Codex Responses model used
// to orchestrate the image_generation tool for an OAuth account. The models
// manifest is already cached by FetchCodexModelsManifest, so this stays aligned
// with the account's live catalog without forcing a network round-trip for every
// image request.
func (s *OpenAIGatewayService) resolveOpenAIImagesResponsesMainModel(ctx context.Context, account *Account) (string, error) {
	legacyFallback := strings.TrimSpace(openAIImagesResponsesMainModel)
	if s == nil || account == nil || !account.IsOpenAIOAuth() {
		return legacyFallback, nil
	}

	manifest, err := s.FetchCodexModelsManifest(ctx, account, codexCLIVersion, "")
	if err != nil {
		// Preserve the pre-existing image path when model discovery is temporarily
		// unavailable. A later upstream request still remains authoritative.
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Images main-model discovery failed for account=%d, fallback=%s: %v",
			account.ID,
			legacyFallback,
			err,
		)
		return legacyFallback, nil
	}
	if manifest == nil || len(manifest.Body) == 0 {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Images main-model discovery returned an empty manifest for account=%d, fallback=%s",
			account.ID,
			legacyFallback,
		)
		return legacyFallback, nil
	}

	manifestModels, err := parseOpenAICodexManifestModels(manifest.Body)
	if err != nil {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Images main-model manifest parse failed for account=%d, fallback=%s: %v",
			account.ID,
			legacyFallback,
			err,
		)
		return legacyFallback, nil
	}
	selected := selectOpenAICodexImageMainModel(manifestModels)
	if selected == "" {
		return "", fmt.Errorf("OpenAI OAuth account has no API-supported image-capable Codex main model")
	}
	if !strings.EqualFold(selected, legacyFallback) {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Images selected dynamic Responses main model=%s account=%d legacy_preference=%s",
			selected,
			account.ID,
			legacyFallback,
		)
	}
	return selected, nil
}

// buildOpenAIImagesResponsesRequestForMainModel keeps the existing image tool
// request builder intact and only replaces the top-level Responses model chosen
// for the current OAuth account.
func buildOpenAIImagesResponsesRequestForMainModel(parsed *OpenAIImagesRequest, mainModel, toolModel string) ([]byte, error) {
	body, err := buildOpenAIImagesResponsesRequest(parsed, toolModel)
	if err != nil {
		return nil, err
	}
	mainModel = strings.TrimSpace(mainModel)
	if mainModel == "" {
		return nil, fmt.Errorf("OpenAI images Responses main model is required")
	}
	body, err = sjson.SetBytes(body, "model", mainModel)
	if err != nil {
		return nil, fmt.Errorf("set OpenAI images Responses main model: %w", err)
	}
	return body, nil
}
