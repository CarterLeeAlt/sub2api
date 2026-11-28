package service

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
)

const OpsNoAvailableAccounts = "no_available_accounts"

type openAIModelsObserverKey struct{}

// ObserveOpenAIModelFailures installs an observer for upstream fetches started by this request.
// Cached responses and callers joining a shared fetch do not start another attempt.
func ObserveOpenAIModelFailures(ctx context.Context, observe func(*OpsUpstreamErrorEvent, string)) context.Context {
	return context.WithValue(ctx, openAIModelsObserverKey{}, observe)
}

func OpenAIModelsErrorPhase(err error) string {
	var upstream *codexModelsManifestUpstreamError
	switch {
	case errors.Is(err, ErrNoAvailableAccounts), errors.Is(err, ErrNoPinnedCodexModelsAccounts):
		return "routing"
	case errors.As(err, &upstream):
		if upstream.transport {
			return "network"
		}
		if upstream.accountAuth {
			return "account_auth"
		}
		return "upstream"
	default:
		return "internal"
	}
}

// OpenAIModelsSelectionDetail exposes only the scheduler's generated candidate counts.
func OpenAIModelsSelectionDetail(err error) string {
	var unavailable openAINoAvailableSelectionError
	if errors.As(err, &unavailable) {
		return unavailable.Error()
	}
	return ""
}

func RecordOpenAIModelsUpstreamError(c *gin.Context, event OpsUpstreamErrorEvent) {
	appendOpsUpstreamError(c, event)
}

func recordOpenAIModelFetch(ctx context.Context, request openAIModelsRequest, err error) error {
	observe, _ := ctx.Value(openAIModelsObserverKey{}).(func(*OpsUpstreamErrorEvent, string))
	endpoint, parseErr := url.Parse(request.url)
	if parseErr != nil {
		return err
	}
	var failure *codexModelsManifestUpstreamError
	if !errors.As(err, &failure) {
		if err == nil && observe != nil {
			observe(nil, endpoint.EscapedPath())
		}
		return err
	}
	failure.accountID = request.accountID
	failure.endpoint = endpoint.EscapedPath()
	if observe == nil {
		return err
	}
	event := OpsUpstreamErrorEvent{
		AtUnixMs: time.Now().UnixMilli(), Platform: PlatformOpenAI, AccountID: request.accountID,
		UpstreamStatusCode: failure.statusCode, UpstreamRequestID: failure.headers.Get("x-request-id"),
		UpstreamURL: endpoint.Host + endpoint.EscapedPath(), Kind: "http_error", Message: failure.Error(),
	}
	if failure.transport {
		event.Kind = "request_error"
		event.Stage = "network"
	}
	if failure.accountAuth {
		event.Stage = string(GatewayFailureStageAccountAuth)
	}
	observe(&event, failure.endpoint)
	return err
}

// OpenAIModelsErrorAttribution identifies the fetch that produced the final error.
func OpenAIModelsErrorAttribution(err error) (int, int64, string) {
	var upstream *codexModelsManifestUpstreamError
	if errors.As(err, &upstream) {
		return upstream.statusCode, upstream.accountID, upstream.endpoint
	}
	return 0, 0, ""
}
