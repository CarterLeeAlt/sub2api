package service

import (
	"context"
	"log/slog"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OpenAIQuotaResetWarningCacheRefreshFailed    = "reset_credit_cache_refresh_failed"
	OpenAIQuotaResetWarningAccountRecoveryFailed = "account_state_recovery_failed"
	OpenAIQuotaResetWarningAccountRefreshFailed  = "account_state_refresh_failed"
)

type openAIQuotaResetWorkflowQuota interface {
	QueryUsage(ctx context.Context, accountID int64) (*OpenAIQuotaUsage, error)
	CachePostResetSnapshot(ctx context.Context, accountID int64, usage *OpenAIQuotaUsage) error
}

type openAIQuotaResetWorkflowRecoverer interface {
	RecoverAccountState(ctx context.Context, accountID int64, options AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error)
}

// OpenAIQuotaResetPostProcessResult summarizes the local recovery performed
// after the explicit reset-quota endpoint successfully consumes one credit.
type OpenAIQuotaResetPostProcessResult struct {
	Quota                 *OpenAIQuotaUsage
	Account               *Account
	CacheRefreshed        bool
	AccountStateRecovered bool
	WarningCode           string
}

// RunOpenAIQuotaResetPostProcess recovers account state, refreshes the
// read-only quota cache and reloads the account row. It has no credit-consuming
// capability; consumption remains confined to the explicit handler call.
func RunOpenAIQuotaResetPostProcess(
	ctx context.Context,
	accountID int64,
	quota openAIQuotaResetWorkflowQuota,
	recoverer openAIQuotaResetWorkflowRecoverer,
	loadAccount func(context.Context, int64) (*Account, error),
) OpenAIQuotaResetPostProcessResult {
	result := OpenAIQuotaResetPostProcessResult{}
	if recoverer == nil {
		result.WarningCode = OpenAIQuotaResetWarningAccountRecoveryFailed
		return result
	}
	if _, err := recoverer.RecoverAccountState(ctx, accountID, AccountRecoveryOptions{InvalidateToken: true}); err != nil {
		slog.Warn("openai_quota_reset_account_recovery_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
		result.WarningCode = OpenAIQuotaResetWarningAccountRecoveryFailed
		return result
	}
	result.AccountStateRecovered = true

	if quota != nil {
		usage, usageErr := quota.QueryUsage(ctx, accountID)
		switch {
		case usageErr != nil || usage == nil:
			slog.Warn("openai_quota_reset_cache_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(usageErr))
			result.WarningCode = OpenAIQuotaResetWarningCacheRefreshFailed
		default:
			if err := quota.CachePostResetSnapshot(ctx, accountID, usage); err != nil {
				slog.Warn("openai_quota_reset_cache_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
				result.WarningCode = OpenAIQuotaResetWarningCacheRefreshFailed
			} else {
				result.Quota = usage
				result.CacheRefreshed = true
			}
		}
	}

	if loadAccount == nil {
		return result
	}
	account, err := loadAccount(ctx, accountID)
	if err != nil {
		slog.Warn("openai_quota_reset_account_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
		if result.WarningCode == "" {
			result.WarningCode = OpenAIQuotaResetWarningAccountRefreshFailed
		}
		return result
	}
	result.Account = account
	return result
}
