package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const openAIResetFenceScope = "openai_reset_account_fence"
const openAIResetRetention = 365 * 24 * time.Hour

type openAIResetFence struct {
	AccountID int64                   `json:"account_id,omitempty"`
	Operation string                  `json:"operation"`
	Result    *OpenAIQuotaResetResult `json:"result,omitempty"`
}

func openAIResetIdentity(fedRAMP bool, workspace string) string {
	return HashIdempotencyKey(fmt.Sprintf("%t:%s", fedRAMP, workspace))
}

// The fence is workspace-wide. A receipt belongs only to the member row that
// redeemed it. Older fences do not identify that row.
func (s *OpenAIQuotaService) latestQuotaRedemption(ctx context.Context, accountID int64, identity string) (string, error) {
	if s.resetIdempotency == nil || s.resetIdempotency.repo == nil {
		return "", nil
	}
	record, err := s.resetIdempotency.repo.GetByScopeAndKeyHash(ctx, openAIResetFenceScope, identity)
	if err != nil || record == nil || record.ResponseBody == nil {
		return "", err
	}
	var fence openAIResetFence
	if err := json.Unmarshal([]byte(*record.ResponseBody), &fence); err != nil {
		return "", err
	}
	if fence.AccountID != accountID || fence.Result == nil || fence.Result.Code != "reset" || fence.Result.WindowsReset <= 0 || fence.Result.Credit == nil {
		return "", nil
	}
	return fence.Result.Credit.RedeemedAt, nil
}

func (s *OpenAIQuotaService) ConfigureRedemption(idem *IdempotencyCoordinator, locks LeaderLockCache) {
	s.resetIdempotency = idem
	s.resetLocks = locks
}

// ResetCredit binds a confirmation to its upstream account and a stable UUID.
// Unknown outcomes retain a durable fence; only the same operation may retry.
func (s *OpenAIQuotaService) ResetCredit(ctx context.Context, id int64, key string) (*OpenAIQuotaResetResult, error) {
	key, err := NormalizeIdempotencyKey(key)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	if s.resetIdempotency == nil || s.resetIdempotency.repo == nil || s.resetLocks == nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	if err := scopedAccountIDAccess(ctx, s.accountRepo, id, "openai.quota.reset", false); err != nil {
		return nil, err
	}
	a, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.IsShadow() {
		return nil, ErrSparkShadowResetNotSupported
	}
	if !a.IsOpenAIOAuth() {
		return nil, infraerrors.BadRequest("OPENAI_RESET_OAUTH_REQUIRED", "OpenAI OAuth account required")
	}
	upstreamID := strings.TrimSpace(a.GetCredential("chatgpt_account_id"))
	if upstreamID == "" {
		upstreamID = strings.TrimSpace(a.GetCredential("organization_id"))
	}
	if upstreamID == "" {
		return nil, infraerrors.BadRequest("OPENAI_RESET_IDENTITY_REQUIRED", "Upstream account identity is required for reset coordination")
	}
	identity := openAIResetIdentity(a.IsChatGPTAccountFedRAMP(), upstreamID)
	operation := HashIdempotencyKey(fmt.Sprintf("openai-reset:%d:%s", id, key))
	result, err := s.resetIdempotency.Execute(ctx, IdempotencyExecuteOptions{
		Scope: "openai_reset_redeem", ActorScope: fmt.Sprintf("account:%d", id), Method: http.MethodPost, Route: "/admin/openai/accounts/:id/reset-quota",
		IdempotencyKey: operation, Payload: map[string]any{"account_id": id, "identity": identity}, TTL: openAIResetRetention, RequireKey: true, ExecutionTimeout: 60 * time.Second,
		RecoverExpiredProcessing: true,
	}, func(exec context.Context) (any, error) {
		return s.redeemResetOnce(exec, id, operation, identity)
	})
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(result.Data)
	if err != nil {
		return nil, err
	}
	var out OpenAIQuotaResetResult
	if err = json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	out.Replayed = out.Replayed || result.Replayed
	return &out, nil
}

func (s *OpenAIQuotaService) redeemResetOnce(ctx context.Context, id int64, operation, identity string) (*OpenAIQuotaResetResult, error) {
	lockKey := "openai:reset-credit:" + identity
	owner := uuid.NewString()
	acquired, err := s.resetLocks.TryAcquireLeaderLock(ctx, lockKey, owner, 90*time.Second)
	if err != nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	if !acquired {
		return nil, infraerrors.Conflict("OPENAI_RESET_BUSY", "Another reset is in progress")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.resetLocks.ReleaseLeaderLock(cleanup, lockKey, owner)
	}()
	repo := s.resetIdempotency.repo
	fence, err := repo.GetByScopeAndKeyHash(ctx, openAIResetFenceScope, identity)
	if err != nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	if fence == nil {
		row := &IdempotencyRecord{Scope: openAIResetFenceScope, IdempotencyKeyHash: identity, RequestFingerprint: identity, Status: IdempotencyStatusProcessing, ExpiresAt: time.Now().Add(openAIResetRetention)}
		if _, err = repo.CreateProcessing(ctx, row); err != nil {
			return nil, ErrIdempotencyStoreUnavail
		}
		fence, err = repo.GetByScopeAndKeyHash(ctx, openAIResetFenceScope, identity)
		if err != nil || fence == nil {
			return nil, ErrIdempotencyStoreUnavail
		}
	}
	var previous openAIResetFence
	if fence.ResponseBody != nil {
		if json.Unmarshal([]byte(*fence.ResponseBody), &previous) != nil || previous.Operation == "" {
			return nil, ErrIdempotencyStoreUnavail
		}
		if previous.Operation == operation && previous.Result != nil {
			out := *previous.Result
			out.Replayed = true
			return &out, nil
		}
		if previous.Operation != operation && previous.Result == nil {
			return nil, infraerrors.Conflict("OPENAI_RESET_UNRESOLVED", "Previous reset is unconfirmed; retry its original confirmation")
		}
	}
	marker := openAIResetFence{AccountID: id, Operation: operation}
	body, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	if err = repo.MarkSucceeded(ctx, fence.ID, http.StatusOK, string(body), time.Now().Add(openAIResetRetention)); err != nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	redeemID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(operation)).String()
	out, err := s.resetCredit(ctx, id, redeemID, identity)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_RESET_UNCONFIRMED", "Reset outcome is unconfirmed; retry the same confirmation: "+err.Error())
	}
	marker.Result = out
	body, err = json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err = repo.MarkSucceeded(cleanup, fence.ID, http.StatusOK, string(body), time.Now().Add(openAIResetRetention)); err != nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	return out, nil
}
