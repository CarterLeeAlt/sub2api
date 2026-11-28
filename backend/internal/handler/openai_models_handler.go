package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) pinnedOpenAIModels(c *gin.Context, group *service.Group) {
	if c.Request.Context().Err() != nil {
		return
	}
	finishObservation := observeOpenAIModelDiscovery(c)
	defer finishObservation()
	if h.openAIGatewayService == nil {
		writeOpenAIModelsError(c, http.StatusInternalServerError, "api_error", "OpenAI model discovery is not configured")
		return
	}
	etag := c.GetHeader("If-None-Match")
	if c.Param("model") != "" {
		etag = "" // A collection ETag cannot validate a single-model representation.
	}
	response, account, err := h.openAIGatewayService.FetchPinnedOpenAIModelsList(
		c.Request.Context(), group, h.maxAccountSwitches, etag,
	)
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		writeOpenAIModelDiscoveryError(c, err, "No available OpenAI model discovery accounts")
		return
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	writeOpenAIModelsResponse(c, response)
	switch c.Writer.Status() {
	case http.StatusNotFound:
		c.Set(opsModelDiscoveryPhaseKey, "request")
	case http.StatusBadGateway:
		c.Set(opsModelDiscoveryPhaseKey, "internal")
	}
}

const (
	opsModelDiscoveryEndpointKey = "ops_model_discovery_endpoint"
	opsModelDiscoveryPhaseKey    = "ops_model_discovery_phase"
	opsModelDiscoveryReasonKey   = "ops_model_discovery_reason"
)

func observeOpenAIModelDiscovery(c *gin.Context) func() {
	c.Set(opsModelDiscoveryEndpointKey, "")
	var mu sync.Mutex
	var events []service.OpsUpstreamErrorEvent
	var endpoint string
	ctx := service.ObserveOpenAIModelFailures(c.Request.Context(), func(event *service.OpsUpstreamErrorEvent, path string) {
		mu.Lock()
		defer mu.Unlock()
		if event != nil {
			events = append(events, *event)
			endpoint = path
		} else if endpoint == "" {
			endpoint = path
		}
	})
	c.Request = c.Request.WithContext(ctx)
	return func() {
		if !c.Writer.Written() {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, event := range events {
			service.RecordOpenAIModelsUpstreamError(c, event)
		}
		if endpoint != "" && c.GetString(opsModelDiscoveryEndpointKey) == "" {
			c.Set(opsModelDiscoveryEndpointKey, endpoint)
		}
	}
}

func writeOpenAIModelDiscoveryError(c *gin.Context, err error, noAccountMessage string) {
	phase := service.OpenAIModelsErrorPhase(err)
	c.Set(opsModelDiscoveryPhaseKey, phase)
	switch phase {
	case "routing":
		c.Set(opsModelDiscoveryReasonKey, service.OpenAIModelsSelectionDetail(err))
		service.SetOpsLocalErrorType(c, service.OpsNoAvailableAccounts, "")
		writeOpenAIModelsError(c, http.StatusServiceUnavailable, "api_error", noAccountMessage)
	case "internal":
		writeOpenAIModelsError(c, infraerrors.Code(err), "api_error", infraerrors.Message(err))
	default:
		status, accountID, endpoint := service.OpenAIModelsErrorAttribution(err)
		c.Set(opsModelDiscoveryEndpointKey, endpoint)
		setOpsSelectedAccount(c, accountID, service.PlatformOpenAI)
		service.SetOpsUpstreamError(c, status, infraerrors.Message(err), "")
		writeOpenAIModelsError(c, infraerrors.Code(err), "upstream_error", infraerrors.Message(err))
	}
}

func writeOpenAIModelsError(c *gin.Context, status int, errorType, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "message": message}})
}

func writeOpenAIModelsResponse(c *gin.Context, manifest *service.OpenAIModelsResponse) {
	if c.Param("model") != "" {
		writeRetrievedModel(c, manifest.Body)
		return
	}
	if manifest.ETag != "" {
		c.Header("ETag", manifest.ETag)
	}
	if manifest.NotModified {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "application/json", manifest.Body)
}

// Both discovery endpoints consume the same final catalogue, after group/platform
// selection and allowlist filtering. Preserve every field on the selected entry.
func writeModelsListResponse(c *gin.Context, models any) {
	response := gin.H{"object": "list", "data": models}
	if c.Param("model") == "" {
		c.JSON(http.StatusOK, response)
		return
	}
	body, err := json.Marshal(response)
	if err != nil {
		writeOpenAIModelsError(c, http.StatusInternalServerError, "api_error", "Failed to encode model catalogue")
		return
	}
	writeRetrievedModel(c, body)
}

func writeRetrievedModel(c *gin.Context, body []byte) {
	var catalog struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		writeOpenAIModelsError(c, http.StatusBadGateway, "upstream_error", "Invalid model catalogue")
		return
	}
	modelID := c.Param("model")
	for _, raw := range catalog.Data {
		var model map[string]json.RawMessage
		if err := json.Unmarshal(raw, &model); err != nil {
			writeOpenAIModelsError(c, http.StatusBadGateway, "upstream_error", "Invalid model catalogue entry")
			return
		}
		var id string
		if err := json.Unmarshal(model["id"], &id); err != nil {
			writeOpenAIModelsError(c, http.StatusBadGateway, "upstream_error", "Invalid model catalogue ID")
			return
		}
		if id == modelID {
			c.Data(http.StatusOK, "application/json", raw)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
		"type": "invalid_request_error", "code": "model_not_found", "param": "model",
		"message": fmt.Sprintf("Model %q does not exist or is not available for this group", modelID),
	}})
}
