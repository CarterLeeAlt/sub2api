package service

import (
	"context"
	"fmt"
	"slices"
	"time"
)

type ClaudeResetRecovery struct {
	ClearGlobal    bool
	ClearThreshold bool
	ClearFable     bool
	Cleared        []string
}

type ClaudeResetRecoveryRepository interface {
	RecoverClaudeReset(context.Context, *Account, ClaudeResetRecovery) (bool, error)
}

func (s *ClaudeResetCreditService) recoverReset(ctx context.Context, account *Account, outcome *ClaudeResetOutcome) error {
	store, ok := s.accounts.(ClaudeResetRecoveryRepository)
	if !ok {
		return fmt.Errorf("claude reset recovery repository is not configured")
	}
	five := slices.Contains(outcome.Cleared, "five_hour")
	seven := slices.Contains(outcome.Cleared, "seven_day")
	recovery := ClaudeResetRecovery{ClearGlobal: five && seven, Cleared: outcome.Cleared}
	if account.RateLimitResetAt != nil {
		if five && account.SessionWindowEnd != nil && account.RateLimitResetAt.Equal(*account.SessionWindowEnd) && account.getExtraFloat64("passive_usage_7d_utilization") < 1 {
			recovery.ClearGlobal = true
		}
		if seven && account.RateLimitResetAt.Unix() == int64(account.getExtraFloat64("passive_usage_7d_reset")) && account.getExtraFloat64("session_window_utilization") < 1 {
			recovery.ClearGlobal = true
		}
	}
	if reason, ok := parseTempUnschedReasonPayload(account.TempUnschedulableReason); ok && reason.Source == AccountSchedulingThresholdReasonSource {
		recovery.ClearThreshold = (reason.Window == "5h" && five) || (reason.Window == "7d" && seven)
	}
	if limits, ok := account.Extra["model_rate_limits"].(map[string]any); ok {
		if fable, ok := limits[anthropicFableRateLimitKey].(map[string]any); ok {
			reason, _ := fable["reason"].(string)
			recovery.ClearFable = slices.Contains(outcome.Cleared, "seven_day_overage_included") && (reason == anthropicFableWindowReason || IsAccountSchedulingThresholdReason(reason))
		}
	}
	applied, err := store.RecoverClaudeReset(ctx, account, recovery)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("account changed during Claude reset; refresh account state")
	}
	if recovery.ClearThreshold && s.rateLimitService != nil && s.rateLimitService.tempUnschedCache != nil {
		if err := s.rateLimitService.tempUnschedCache.DeleteTempUnsched(ctx, account.ID); err != nil {
			return err
		}
	}
	if s.rateLimitService != nil {
		s.rateLimitService.notifyAccountSchedulingBlockCleared(account.ID)
	}
	return nil
}

func claudeResetRecoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
