package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// DiagnoseEmptyOpenAISelectionPool counts current database state after the
// scheduler reported an empty pool. The result is only for the failure log;
// the database read does not participate in selection or error classification.
func (s *OpenAIGatewayService) DiagnoseEmptyOpenAISelectionPool(ctx context.Context, groupID *int64, model string, requireCompact bool, selectionErr error) (map[string]int, error) {
	var empty openAINoAvailableSelectionError
	if !errors.As(selectionErr, &empty) || !empty.emptyPool || s.accountRepo == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var accounts []Account
	var err error
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListActive(ctx)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListByGroup(ctx, *groupID)
	} else {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	counts := map[string]int{"active_accounts": 0}
	for i := range accounts {
		account := &accounts[i]
		if !account.IsOpenAI() {
			continue
		}
		counts["active_accounts"]++
		switch {
		case !account.Schedulable:
			counts["manually_paused"]++
		case account.ModelCatalogPending():
			counts["catalog_pending"]++
		case account.IsRateLimited():
			counts["account_rate_limited"]++
		case account.IsOverloaded():
			counts["overloaded"]++
		case !account.IsSchedulable():
			counts["other_account_blocks"]++
		case !supportsOpenAIRequestModel(account, model, requireCompact):
			counts["model_not_supported"]++
		default:
			counts["passes_account_and_model_checks"]++
		}
	}
	return counts, nil
}

// DiagnoseModelAvailabilityForPlatform reports whether the requested model
// is configured to be served by any persistently eligible OpenAI-compatible
// account in the group for the given platform (e.g. PlatformOpenAI,
// PlatformGrok). The platform scopes the candidate pool so distinct
// OpenAI-compatible platforms do not cross-contaminate diagnosis results.
// The query bypasses scheduler snapshots and ignores transient runtime state.
//
// Safe to call on the error path: returns {true,true} on any internal
// failure or when the inputs preclude meaningful diagnosis (empty model,
// nil service), so callers stay on the 503 fallback branch.
func (s *OpenAIGatewayService) DiagnoseModelAvailabilityForPlatform(
	ctx context.Context,
	groupID *int64,
	requestedModel string,
	platform string,
) ModelAvailabilityDiagnosis {
	if s == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	if s.accountRepo == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	platform = NormalizeOpenAICompatiblePlatform(platform)
	queryGroupID := groupID
	includeGrouped := false
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		queryGroupID = nil
		includeGrouped = true
	}
	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(
		ctx,
		queryGroupID,
		[]string{platform},
		includeGrouped,
	)
	if err != nil {
		// Conservative fallback so the caller keeps returning 503; we do not
		// want a transient lookup failure to flip into 404 model_not_found.
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	diag := ModelAvailabilityDiagnosis{}
	for i := range accounts {
		diag.HasAccountsInPool = true
		if isRetiredOpenAISubscriptionModel(&accounts[i], resolveOpenAIAccountUpstreamModelForRequest(&accounts[i], requestedModel, false)) {
			diag.HasRetiredSubscriptionModel = true
		}
		// Mirrors the per-candidate filter used during account selection
		// (openai_account_scheduler.isAccountRequestCompatible): empty
		// model_mapping accepts everything; otherwise the explicit / wildcard
		// mapping must match.
		if accounts[i].IsModelSupported(requestedModel) {
			if toolRequestNeedsDifferentProtocol(ctx, &accounts[i], requestedModel, false) {
				diag.HasToolProtocolMismatch = true
				continue
			}
			diag.HasModelSupport = true
			account := &accounts[i]
			diag.AllSupportingAccountsRateLimited = account.IsRateLimited() || account.isModelRateLimitedWithContext(ctx, requestedModel)
			if !diag.AllSupportingAccountsRateLimited {
				return diag
			}
		}
	}
	return diag
}
