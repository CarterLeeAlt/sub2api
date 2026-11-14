package service

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"
)

const ClaudeOAuthStateKey = "claude_oauth_state"

// OAuthCredentialIdentity includes absent fields so a concurrent identity change
// cannot pass the comparison by adding a previously missing field.
func OAuthCredentialIdentity(credentials map[string]any) map[string]any {
	identity := make(map[string]any)
	for _, key := range []string{"access_token", "refresh_token", "id_token", "_token_version", "client_id", "chatgpt_account_id", "organization_id", "account_uuid", "organization_uuid", "is_fedramp", "scope"} {
		identity[key] = credentials[key]
	}
	return identity
}

type OAuthCredentialRepository interface {
	UpdateOAuthCredentialsIfUnchanged(context.Context, *Account, map[string]any) (bool, error)
	SaveOAuthAuthenticationState(context.Context, *Account, OpenAIUpstreamHealth, time.Time) (bool, error)
}

func usesConditionalOAuthState(account *Account) bool {
	return account != nil && account.IsOAuth() &&
		(account.Platform == PlatformAnthropic || (account.Platform == PlatformOpenAI && !account.IsOpenAIAgentIdentity()))
}

func claudeOAuthRefreshRequired(account *Account) bool {
	if account == nil {
		return false
	}
	state, _ := account.Extra[ClaudeOAuthStateKey].(map[string]any)
	return state["kind"] == "auth_refresh_required"
}

type oauthRequestIdentityKey struct{}

// Bind the actual bearer to the request context. A shadow's owner is captured
// before sending, never resolved again from a newer row when an error arrives.
func withOAuthRequestIdentity(ctx context.Context, repo AccountRepository, account *Account, token string) context.Context {
	if !usesConditionalOAuthState(account) || token == "" {
		return ctx
	}
	owner, err := resolveCredentialAccount(ctx, repo, account)
	if err != nil || owner == nil {
		return ctx
	}
	if owner.GetCredential("access_token") != token && repo != nil {
		if latest, err := repo.GetByID(ctx, owner.ID); err == nil && latest != nil && latest.GetCredential("access_token") == token {
			copy := *latest
			copy.ProxyID = owner.ProxyID
			owner = &copy
		}
	}
	snapshot := snapshotOAuthRefreshAccount(owner)
	snapshot.Credentials = maps.Clone(owner.Credentials)
	if snapshot.Credentials == nil {
		snapshot.Credentials = map[string]any{}
	}
	snapshot.Credentials["access_token"] = token
	return context.WithValue(ctx, oauthRequestIdentityKey{}, snapshot)
}

func oauthRequestIdentity(ctx context.Context, account *Account) *Account {
	if snapshot, ok := ctx.Value(oauthRequestIdentityKey{}).(*Account); ok &&
		(snapshot.ID == account.ID || (account.ParentAccountID != nil && snapshot.ID == *account.ParentAccountID)) {
		return snapshot
	}
	return account
}

func (s *RateLimitService) handleOAuthAuthenticationFailure(ctx context.Context, account *Account, status int, body []byte) (bool, bool) {
	if !usesConditionalOAuthState(account) {
		return false, false
	}
	identity := oauthRequestIdentity(ctx, account)
	if status != http.StatusUnauthorized && (account.Platform != PlatformOpenAI || !isOpenAIHTTPUpstreamAccessStateError(status, "", body)) {
		return false, false
	}
	state := classifyOpenAIUpstreamHealth(identity, "gateway", status, body)
	if identity.Platform == PlatformAnthropic {
		state.Kind = "auth_refresh_required"
		if strings.TrimSpace(identity.GetCredential("refresh_token")) == "" {
			state.Kind = "auth_invalid"
		}
	}
	applied := s.saveOAuthAuthenticationFailure(ctx, identity, state)
	if applied {
		s.maybeHandleOpenAITeamLinkedError(ctx, identity, status, body)
	}
	return true, applied
}

func (s *RateLimitService) saveOAuthAuthenticationFailure(ctx context.Context, identity *Account, state OpenAIUpstreamHealth) bool {
	store, ok := s.accountRepo.(OAuthCredentialRepository)
	if !ok || identity.IsShadow() {
		return false
	}
	ctx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	minutes := 10
	if s.cfg != nil && s.cfg.RateLimit.OAuth401CooldownMinutes > 0 {
		minutes = s.cfg.RateLimit.OAuth401CooldownMinutes
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	applied, err := store.SaveOAuthAuthenticationState(ctx, identity, state, until)
	if err != nil {
		slog.Warn("oauth_auth_state_write_failed", "account_id", identity.ID, "error", err)
		return false
	}
	// Durable snapshots/outbox own scheduling. A late in-memory block by ID could
	// race with a subsequent reauthorization after the conditional write commits.
	if applied && s.tokenCacheInvalidator != nil {
		_ = s.tokenCacheInvalidator.InvalidateToken(ctx, identity)
	}
	return applied
}
