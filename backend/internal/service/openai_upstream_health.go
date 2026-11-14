package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const OpenAIUpstreamHealthExtraKey = "openai_upstream_health"

// OpenAIUpstreamHealth separates account authentication from an operation failure.
// Only an authoritative credential failure can change account scheduling state.
type OpenAIUpstreamHealth struct {
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	ObservedAt string `json:"observed_at"`
}

type OpenAIUpstreamHealthRepository interface {
	SaveOpenAIUpstreamHealth(context.Context, *Account, OpenAIUpstreamHealth) (bool, error)
}

func classifyOpenAIUpstreamHealth(account *Account, source string, status int, body []byte) OpenAIUpstreamHealth {
	code := strings.ToLower(extractUpstreamErrorCode(body))
	message := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if message == "" {
		message = http.StatusText(status)
	}
	if message == "" {
		message = "Upstream request failed"
	}
	if account != nil {
		for _, key := range []string{"access_token", "refresh_token", "id_token", "api_key", "agent_private_key"} {
			if secret := account.GetCredential(key); secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
	}
	state := OpenAIUpstreamHealth{Kind: "request_failed", Source: source, Code: code, Message: truncate(SanitizeUpstreamErrorMessage(message), 1024), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	switch {
	case status == 0:
		state.Kind = "network_error"
	case status == 429:
		state.Kind = "rate_limited"
	case status == 402:
		state.Kind = "quota_exhausted"
	case status == 403:
		state.Kind = "permission_denied"
	}
	if account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIAgentIdentity() {
		return state
	}
	if isOpenAIHTTPUpstreamAccessStateError(status, "", body) || (status == 401 && (code == "token_invalidated" || code == "token_revoked" || gjson.GetBytes(body, "detail").String() == "Unauthorized" || strings.Contains(strings.ToLower(message), "invalidated oauth token"))) {
		state.Kind = "auth_invalid"
	} else if status == 401 {
		if source == "reset_credits" {
			state.Kind = "permission_denied"
			return state
		}
		state.Kind = "auth_refresh_required"
		if strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" || account.IsOpenAIPersonalAccessToken() {
			state.Kind = "auth_invalid"
		}
	}
	return state
}

// observeOpenAIUpstreamResponse uses the credential snapshot that actually sent the
// request. The repository compares it atomically; a late error cannot disable a
// newly authorized account. Success clears only this source's diagnostic.
func observeOpenAIUpstreamResponse(ctx context.Context, repo AccountRepository, account *Account, source string, status int, body []byte) error {
	if account == nil || !account.IsOpenAIOAuth() {
		return nil
	}
	state := classifyOpenAIUpstreamHealth(account, source, status, body)
	if status >= 200 && status < 300 {
		state.Kind = "ok"
		state.Message = ""
		state.Code = ""
	}
	if store, ok := repo.(OpenAIUpstreamHealthRepository); ok {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := store.SaveOpenAIUpstreamHealth(writeCtx, account, state); err != nil {
			slog.Warn("openai_upstream_health_persist_failed", "account_id", account.ID, "source", source, "error", err)
		}
	}
	if state.Kind == "ok" {
		return nil
	}
	// Upstream authentication belongs to the managed account, not the admin login.
	return infraerrors.New(http.StatusBadGateway, "OPENAI_UPSTREAM_"+strings.ToUpper(state.Kind), fmt.Sprintf("%s: %s", state.Kind, state.Message))
}

func captureOpenAIQuotaIdentity(ctx context.Context, repo AccountRepository, id int64, accessToken, chatGPTAccountID, proxyURL string, fedRAMP bool) (*Account, error) {
	account, err := repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	account, err = resolveCredentialAccount(ctx, repo, account)
	if err != nil {
		return nil, err
	}
	currentID := strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	if currentID == "" {
		currentID = strings.TrimSpace(account.GetCredential("organization_id"))
	}
	currentProxy := ""
	if account.Proxy != nil {
		currentProxy = account.Proxy.URL()
	}
	if currentID != chatGPTAccountID || account.IsChatGPTAccountFedRAMP() != fedRAMP || (account.Proxy != nil && currentProxy != proxyURL) || (account.ProxyID == nil && proxyURL != "") {
		return nil, infraerrors.Conflict("OPENAI_CREDENTIALS_CHANGED", "Account connection changed; retry the query")
	}
	if account.IsOpenAIAgentIdentity() {
		return account, nil
	}
	copy := *account
	copy.Credentials = maps.Clone(account.Credentials)
	if copy.Credentials == nil {
		copy.Credentials = map[string]any{}
	}
	copy.Credentials["access_token"] = accessToken
	return &copy, nil
}

func openAIHealthErrorKind(err error) string {
	reason := infraerrors.Reason(err)
	if !strings.HasPrefix(reason, "OPENAI_UPSTREAM_") {
		return "request_failed"
	}
	return strings.ToLower(strings.TrimPrefix(reason, "OPENAI_UPSTREAM_"))
}

func accountOpenAIUpstreamHealth(account *Account) OpenAIUpstreamHealth {
	var state OpenAIUpstreamHealth
	if account != nil {
		value, _ := account.Extra[OpenAIUpstreamHealthExtraKey].(map[string]any)
		state.Kind, _ = value["kind"].(string)
		state.Source, _ = value["source"].(string)
		state.Code, _ = value["code"].(string)
		state.Message, _ = value["message"].(string)
		state.ObservedAt, _ = value["observed_at"].(string)
	}
	return state
}
