package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

const officialImageSpecURL = "https://raw.githubusercontent.com/openai/openai-openapi/main/openapi.json"
const imageSpecSetting = "openai_images_openapi_catalog"

type imageEndpointSupport struct {
	Generate bool `json:"generate,omitempty"`
	Edit     bool `json:"edit,omitempty"`
}

type imageSpecSnapshot struct {
	Models  map[string]imageEndpointSupport `json:"models"`
	ETag    string                          `json:"etag"`
	Checked time.Time                       `json:"checked"`
	Updated time.Time                       `json:"updated"`
}

// Immutable snapshots are shared by parsing, routing and account discovery.
var officialImageModels atomic.Pointer[imageSpecSnapshot]

func parseOfficialImageModels(body []byte) (map[string]imageEndpointSupport, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("invalid OpenAI OpenAPI JSON")
	}
	root := gjson.ParseBytes(body)
	models := make(map[string]imageEndpointSupport)
	for _, endpoint := range []string{"generations", "edits"} {
		schema := root.Get("paths./images/" + endpoint + ".post.requestBody.content.application/json.schema")
		resolve := func(node gjson.Result) gjson.Result {
			if name, ok := strings.CutPrefix(node.Get("$ref").String(), "#/components/schemas/"); ok {
				return root.Get("components.schemas." + name)
			}
			return node
		}
		field := resolve(resolve(schema).Get("properties.model"))
		count := 0
		for _, branch := range append(field.Get("anyOf").Array(), field) {
			branch = resolve(branch)
			deprecations := branch.Get("x-openai-enum-deprecations").Map()
			for _, entry := range branch.Get("enum").Array() {
				id := entry.String()
				if entry.Type != gjson.String || id == "" || strings.ContainsAny(id, "* /\t\n\r") {
					return nil, errors.New("invalid image model ID in official catalog")
				}
				message := strings.ToLower(deprecations[id].Get("message").String())
				if strings.Contains(message, "was shut down") || strings.Contains(message, "was retired") {
					continue
				}
				support := models[id]
				if endpoint == "generations" {
					support.Generate = true
				} else {
					support.Edit = true
				}
				models[id] = support
				count++
			}
		}
		if count == 0 {
			return nil, fmt.Errorf("OpenAI /images/%s model enum is empty", endpoint)
		}
	}
	return models, nil
}

func fetchOfficialImageSpec(ctx context.Context, client *http.Client, url string, previous imageSpecSnapshot, now time.Time) (imageSpecSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return previous, err
	}
	if len(previous.Models) > 0 && previous.ETag != "" {
		req.Header.Set("If-None-Match", previous.ETag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return previous, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && len(previous.Models) > 0 {
		previous.Checked = now
		return previous, nil
	}
	if resp.StatusCode != http.StatusOK {
		return previous, fmt.Errorf("OpenAI OpenAPI download: HTTP %d", resp.StatusCode)
	}
	const limit = 24 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return previous, err
	}
	if len(body) > limit {
		return previous, errors.New("OpenAI OpenAPI document too large")
	}
	models, err := parseOfficialImageModels(body)
	if err != nil {
		return previous, err
	}
	return imageSpecSnapshot{Models: models, ETag: resp.Header.Get("ETag"), Checked: now, Updated: now}, nil
}

// Only the image catalog leader contacts OpenAI. Other instances load the
// compact snapshot from settings so request handling never downloads the spec.
func (s *OpenAIModelCatalogRefreshService) refreshImageSpec(ctx context.Context) error {
	const lockKey = "openai:image-catalog:refresh"
	locked, err := s.locks.TryAcquireLeaderLock(ctx, lockKey, s.owner, time.Minute)
	if err != nil {
		return err
	}
	if locked {
		defer func() {
			release, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = s.locks.ReleaseLeaderLock(release, lockKey, s.owner)
		}()
	}
	var snapshot imageSpecSnapshot
	raw, err := s.settings.GetValue(ctx, imageSpecSetting)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return err
	}
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &snapshot); err != nil {
			return err
		}
		if len(snapshot.Models) > 0 {
			copy := snapshot
			officialImageModels.Store(&copy)
		}
	}
	if !locked || time.Since(snapshot.Checked) < time.Hour || time.Now().Before(s.nextImageSpecFetch) {
		return nil
	}
	now := time.Now().UTC()
	s.nextImageSpecFetch = now.Add(time.Hour)
	updated, fetchErr := fetchOfficialImageSpec(ctx, &http.Client{Timeout: 25 * time.Second}, officialImageSpecURL, snapshot, now)
	// Persist failed checks too. A change of leader must not start a second
	// download within the hour or replace the last usable catalog.
	updated.Checked = now
	encoded, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	if err = s.settings.Set(ctx, imageSpecSetting, string(encoded)); err != nil {
		return err
	}
	if len(updated.Models) > 0 {
		officialImageModels.Store(&updated)
	}
	return fetchErr
}
