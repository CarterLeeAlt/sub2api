package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResetRetryAndReplayConsumeOnce(t *testing.T) {
	a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "fake-token", "chatgpt_account_id": "upstream-account"}}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: a, 101: {ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: a.Credentials}}}
	cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(a): "fake-token"}}
	ids := []string{}
	consumed := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		id := body["redeem_request_id"]
		ids = append(ids, id)
		consumed[id] = true
		if len(ids) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
	}))
	defer server.Close()
	svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(server), nil)
	cfg := DefaultIdempotencyConfig()
	cfg.FailedRetryBackoff = 0
	svc.ConfigureRedemption(NewIdempotencyCoordinator(newInMemoryIdempotencyRepo(), cfg), &redeemLeaseStub{})
	_, err := svc.ResetCredit(context.Background(), 100, "")
	require.ErrorIs(t, err, ErrIdempotencyKeyRequired)
	_, err = svc.ResetCredit(context.Background(), 100, "confirmation")
	require.Equal(t, "OPENAI_RESET_UNCONFIRMED", infraerrors.Reason(err))
	_, err = svc.ResetCredit(context.Background(), 101, "different-confirmation")
	require.Equal(t, "OPENAI_RESET_UNRESOLVED", infraerrors.Reason(err))
	require.Len(t, ids, 1)
	out, err := svc.ResetCredit(context.Background(), 100, "confirmation")
	require.NoError(t, err)
	require.Equal(t, 2, out.WindowsReset)
	replay, err := svc.ResetCredit(context.Background(), 100, "confirmation")
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Len(t, ids, 2)
	require.Equal(t, ids[0], ids[1])
	require.Len(t, consumed, 1)
}

// A crash or failed final write leaves the outer operation processing even when
// the irreversible result has already been saved in the account/org fence.
type resetCompletionFailureRepo struct {
	*inMemoryIdempotencyRepo
	failScope string
}

func (r *resetCompletionFailureRepo) MarkSucceeded(ctx context.Context, id int64, status int, body string, expires time.Time) error {
	r.mu.Lock()
	fail := false
	for _, record := range r.data {
		if record.ID == id && record.Scope == r.failScope {
			fail = true
			r.failScope = ""
			break
		}
	}
	r.mu.Unlock()
	if fail {
		return errors.New("simulated operation completion write failure")
	}
	return r.inMemoryIdempotencyRepo.MarkSucceeded(ctx, id, status, body, expires)
}

func expireResetOperationLease(repo *inMemoryIdempotencyRepo, scope string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, record := range repo.data {
		if record.Scope == scope {
			past := time.Now().Add(-time.Minute)
			record.LockedUntil = &past
		}
	}
}

func TestOpenAIResetRecoversLostOperationCompletion(t *testing.T) {
	a := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"access_token": "fake-token", "chatgpt_account_id": "upstream-account"}}
	accounts := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: a}}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
	}))
	defer server.Close()
	repo := &resetCompletionFailureRepo{inMemoryIdempotencyRepo: newInMemoryIdempotencyRepo(), failScope: "openai_reset_redeem"}
	svc := NewOpenAIQuotaService(accounts, nil, NewOpenAITokenProvider(accounts, nil, nil), newQuotaRedirectingFactory(server), nil)
	svc.ConfigureRedemption(NewIdempotencyCoordinator(repo, DefaultIdempotencyConfig()), &redeemLeaseStub{})
	_, err := svc.ResetCredit(context.Background(), 100, "lost-completion")
	require.ErrorIs(t, err, ErrIdempotencyStoreUnavail)
	_, err = svc.ResetCredit(context.Background(), 100, "lost-completion")
	require.ErrorIs(t, err, ErrIdempotencyInProgress, "a live operation lease cannot be reclaimed")
	expireResetOperationLease(repo.inMemoryIdempotencyRepo, "openai_reset_redeem")
	out, err := svc.ResetCredit(context.Background(), 100, "lost-completion")
	require.NoError(t, err)
	require.True(t, out.Replayed)
	require.Equal(t, 2, out.WindowsReset)
	require.Equal(t, 1, posts, "recovery must read the durable fence without another redemption")
}
