package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const fixtureImageSpec = `{"paths":{"/images/generations":{"post":{"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Generate"}}}}}},"/images/edits":{"post":{"requestBody":{"content":{"application/json":{"schema":{"properties":{"model":{"enum":["fixture-painter"]}}}}}}}}},"components":{"schemas":{"Generate":{"properties":{"model":{"anyOf":[{"type":"string"},{"enum":["fixture-painter","fixture-retired"],"x-openai-enum-deprecations":{"fixture-retired":{"message":"This model was retired on July 2."}}}]}}}}}}`

const fixtureImageDeprecations = "## Upcoming deprecations\n\n" +
	"| Shutdown date | Affected model | Status | Recommended replacement |\n" +
	"| --- | --- | --- | --- |\n" +
	"| **Dec 1, 2026** | `gpt-image-1-mini`, `gpt-image-1.5`, `chatgpt-image-latest` | Deprecated | `gpt-image-2.6-flare` |\n" +
	"| **Feb 10, 2027** | `gpt-image-2`, `gpt-image-2-2026-04-21`, `fixture-driver` | Deprecated | `gpt-image-2.6-sunburst` |\n\n" +
	"## Feature and endpoint deprecations\n" +
	"| Feb 15, 2027 | `gpt-image-2.5-flare` | Deprecated | Test feature notice |\n\n" +
	"## Past deprecations\n\n" +
	"| Shutdown date | Affected model or feature | Listed replacement |\n" +
	"| --- | --- | --- |\n" +
	"| **Oct 23, 2026** | `gpt-image-1` | `gpt-image-2.5-flare` |\n"

func TestOfficialImageCatalogSharedHourlyCheck(t *testing.T) {
	previous := officialImageModels.Swap(nil)
	defer officialImageModels.Store(previous)
	now := time.Now().UTC()
	cache, err := json.Marshal(imageSpecSnapshot{Models: map[string]imageEndpointSupport{"fixture-painter": {Generate: true}}, Lifecycle: &imageModelLifecycle{}, Checked: now.Add(-59 * time.Minute), Updated: now.Add(-24 * time.Hour)})
	require.NoError(t, err)
	settings := newCodexVersionSyncSettingRepoStub(map[string]string{imageSpecSetting: string(cache)})
	for _, leader := range []bool{false, true, true} {
		locks := &fakeLeaderLockCache{}
		if !leader {
			_, _ = locks.TryAcquireLeaderLock(t.Context(), "openai:image-catalog:refresh", "other", time.Minute)
		}
		worker := OpenAIModelCatalogRefreshService{settings: settings, locks: locks, owner: "local"}
		require.NoError(t, worker.refreshImageSpec(t.Context(), nil))
		require.Empty(t, settings.syncedWrites())
		require.Zero(t, worker.nextImageSpecFetch)
		require.True(t, officialImageModels.Load().Models["fixture-painter"].Generate)
	}
}

func TestOfficialImageCatalogInitialFetchFailureSharesRetryInterval(t *testing.T) {
	previous := officialImageModels.Swap(nil)
	defer officialImageModels.Store(previous)
	settings := newCodexVersionSyncSettingRepoStub(nil)
	unavailable := errors.New("fixture source unavailable")
	fetch := func(_ context.Context, previous imageSpecSnapshot, _ time.Time) (imageSpecSnapshot, error) {
		return previous, unavailable
	}
	first := OpenAIModelCatalogRefreshService{settings: settings, locks: &fakeLeaderLockCache{}, owner: "first"}
	require.ErrorIs(t, first.refreshImageSpec(t.Context(), fetch), unavailable)
	second := OpenAIModelCatalogRefreshService{settings: settings, locks: &fakeLeaderLockCache{}, owner: "second"}
	require.NoError(t, second.refreshImageSpec(t.Context(), nil))
	require.Len(t, settings.syncedWrites(), 1)
	require.Nil(t, officialImageModels.Load())
}

func TestOfficialImageCatalogHTTP(t *testing.T) {
	status := http.StatusOK
	body := fixtureImageSpec
	var conditional string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conditional = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"spec"`)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	now := time.Now().UTC()
	first, err := fetchOfficialImageSpec(t.Context(), server.Client(), server.URL, imageSpecSnapshot{}, now)
	require.NoError(t, err)
	require.Empty(t, conditional)
	require.Equal(t, map[string]imageEndpointSupport{"fixture-painter": {Generate: true, Edit: true}}, first.Models)
	status = http.StatusNotModified
	next, err := fetchOfficialImageSpec(t.Context(), server.Client(), server.URL, first, now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, `"spec"`, conditional)
	require.Equal(t, first.Updated, next.Updated)
	require.Equal(t, now.Add(time.Hour), next.Checked)
	for _, invalid := range []string{"", `{}`, strings.ReplaceAll(fixtureImageSpec, "/images/edits", "/broken")} {
		status, body = http.StatusOK, invalid
		got, err := fetchOfficialImageSpec(t.Context(), server.Client(), server.URL, first, now)
		require.Error(t, err)
		require.Equal(t, first, got)
	}
	status = http.StatusBadGateway
	got, err := fetchOfficialImageSpec(t.Context(), server.Client(), server.URL, first, now)
	require.Error(t, err)
	require.Equal(t, first, got)
}

func TestOAuthImageSyncFiltersDeprecationsAndDefaultSnapshots(t *testing.T) {
	active := []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst"}
	imageIDs := append(append([]string{}, active...), "gpt-image-2.5-flare-2026-09-08", "gpt-image-2.5-sunburst-2026-09-08",
		"gpt-image-1-mini", "gpt-image-1.5", "chatgpt-image-latest", "gpt-image-2", "gpt-image-2-2026-04-21")
	spec := strings.ReplaceAll(fixtureImageSpec, `"fixture-painter"`, `"`+strings.Join(imageIDs, `","`)+`"`)
	deprecations := fixtureImageDeprecations
	flareSnapshot := "gpt-image-2.5-flare-2026-09-08"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			w.Header().Set("ETag", `"spec"`)
			if r.Header.Get("If-None-Match") == `"spec"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = io.WriteString(w, spec)
		case "/deprecations.md":
			_, _ = io.WriteString(w, deprecations)
		case "/models/gpt-image-2.5-flare.md":
			_, _ = fmt.Fprintf(w, "# GPT Image\n\nModel ID: `gpt-image-2.5-flare`\n\n- Default snapshot: `%s`\n", flareSnapshot)
		case "/models/gpt-image-2.5-sunburst.md":
			_, _ = io.WriteString(w, "# GPT Image\n\nModel ID: `gpt-image-2.5-sunburst`\n\n- Default snapshot: `gpt-image-2.5-sunburst-2026-09-08`\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Now().UTC()
	previous := officialImageModels.Swap(nil)
	defer officialImageModels.Store(previous)
	oldModels, err := parseOfficialImageModels([]byte(spec))
	require.NoError(t, err)
	// The previous release wrote a fresh spec-only snapshot without lifecycle data.
	oldCache, err := json.Marshal(map[string]any{"models": oldModels, "etag": `"spec"`, "checked": now.Add(-20 * time.Minute), "updated": now.Add(-time.Hour)})
	require.NoError(t, err)
	settings := newCodexVersionSyncSettingRepoStub(map[string]string{imageSpecSetting: string(oldCache)})
	loader := OpenAIModelCatalogRefreshService{settings: settings, locks: &fakeLeaderLockCache{}, owner: "upgrade"}
	require.NoError(t, loader.refreshImageSpec(t.Context(), func(ctx context.Context, previous imageSpecSnapshot, now time.Time) (imageSpecSnapshot, error) {
		return fetchOfficialImageCatalog(ctx, server.Client(), server.URL+"/openapi.json", server.URL, previous, now)
	}))
	first := officialImageModels.Load()
	require.NotNil(t, first)
	require.NotNil(t, first.Lifecycle)
	require.Len(t, settings.syncedWrites(), 1)
	require.Equal(t, active, first.Lifecycle.filterModelIDs(imageIDs))

	account := directImagesTestAccount()
	account.Credentials["plan_type"] = "team"
	SetAccountModelCatalog(account, AccountModelCatalog{Source: "upstream", Revision: "initial", Ready: true})
	ReplaceAccountModelWhitelist(account, imageIDs)
	store := &catalogRefreshTestStore{accounts: []Account{*account}}
	manifest := `{"models":[{"slug":"fixture-driver","supported_in_api":true,"input_modalities":["text","image"],"reasoning":false,"context_window":1000},{"slug":"gpt-image-2"},{"slug":"gpt-image-1"}]}`
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(manifest))}, nil
	}}
	tester := &AccountTestService{httpUpstream: upstream}
	manual, err := tester.SyncUpstreamModelCatalog(t.Context(), account)
	require.NoError(t, err)
	models := append([]string{"fixture-driver"}, active...)
	require.Equal(t, models, manual.Models)
	worker := NewOpenAIModelCatalogRefreshService(store, tester, &fakeLeaderLockCache{})
	defer worker.Stop()
	require.NoError(t, worker.RunOnce(t.Context()))
	require.Len(t, store.saved, 1)
	for _, id := range models {
		require.True(t, store.saved[0].IsModelSupported(id), id)
	}
	for _, id := range append(imageIDs[len(active):], "gpt-image-1") {
		require.False(t, store.saved[0].IsModelSupported(id), id)
	}

	// A newer default does not make the previous snapshot a duplicate.
	flareSnapshot = "gpt-image-2.5-flare-2026-12-01"
	next, err := fetchOfficialImageCatalog(t.Context(), server.Client(), server.URL+"/openapi.json", server.URL, *first, now.Add(time.Hour))
	require.NoError(t, err)
	require.Contains(t, next.Lifecycle.filterModelIDs(imageIDs), "gpt-image-2.5-flare-2026-09-08")
	require.Equal(t, []string{"gpt-image-2.5-sunburst-2026-09-08"}, first.Lifecycle.filterModelIDs([]string{"gpt-image-2.5-sunburst-2026-09-08"}))

	deprecations = strings.ReplaceAll(fixtureImageDeprecations, "Affected model", "Rearranged column")
	retained, err := fetchOfficialImageCatalog(t.Context(), server.Client(), server.URL+"/openapi.json", server.URL, *first, now.Add(time.Hour))
	require.Error(t, err)
	require.Equal(t, *first, retained)
	deprecations = fixtureImageDeprecations
	flareSnapshot = ""
	retained, err = fetchOfficialImageCatalog(t.Context(), server.Client(), server.URL+"/openapi.json", server.URL, *first, now.Add(time.Hour))
	require.Error(t, err)
	require.Equal(t, *first, retained)

	officialImageModels.Store(&imageSpecSnapshot{Models: first.Models})
	_, err = tester.SyncUpstreamModelCatalog(t.Context(), account)
	require.ErrorContains(t, err, "image catalog is not available yet")
}

func TestOAuthSyncListsOfficialImageModelsWithoutGeneratingImages(t *testing.T) {
	models, err := parseOfficialImageModels([]byte(fixtureImageSpec))
	require.NoError(t, err)
	models["withheld-painter"] = imageEndpointSupport{Generate: true}
	models["editing-only"] = imageEndpointSupport{Edit: true}
	previous := officialImageModels.Swap(&imageSpecSnapshot{Models: models, Lifecycle: &imageModelLifecycle{}, Updated: time.Now()})
	defer officialImageModels.Store(previous)
	a := directImagesTestAccount()
	a.Credentials["plan_type"] = "team"
	SetAccountModelCatalog(a, AccountModelCatalog{Source: "upstream", Revision: "original", Ready: true})
	store := &catalogRefreshTestStore{accounts: []Account{*a}}
	const model = "fixture-painter"
	imageRequests := 0
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
		response := `{"data":[{"b64_json":"aGVsbG8="}]}`
		if req.Method == http.MethodGet {
			response = `{"models":[{"slug":"driver","supported_in_api":true,"input_modalities":["text","image"]},{"slug":"withheld-painter","supported_in_api":false}]}`
		} else {
			imageRequests++
			require.Equal(t, a.ID, accountID)
			require.Equal(t, "/backend-api/codex/images/generations", req.URL.Path)
			payload, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, model, gjson.GetBytes(payload, "model").String())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
	}}
	tester := &AccountTestService{httpUpstream: upstream}
	for _, plan := range []string{"free", "", "team"} {
		a.Credentials["plan_type"] = plan
		ids, err := tester.FetchUpstreamSupportedModels(t.Context(), a)
		require.NoError(t, err)
		if plan == "team" {
			require.Equal(t, []string{"driver", model}, ids)
		} else {
			require.Equal(t, []string{"driver"}, ids)
		}
	}
	worker := NewOpenAIModelCatalogRefreshService(store, tester, &fakeLeaderLockCache{})
	defer worker.Stop()
	require.NoError(t, worker.RunOnce(t.Context()))
	require.Zero(t, imageRequests)
	require.Len(t, store.saved, 1)
	synced := store.saved[0]
	require.True(t, synced.IsModelSupported(model))
	require.False(t, synced.IsModelSupported("withheld-painter"))
	req := []byte(fmt.Sprintf(`{"model":%q,"prompt":"circle"}`, model))
	c, _ := newOpenAIImagesTestContext(t, req)
	gateway := newOpenAIImagesTestService(upstream)
	parsed, err := gateway.ParseOpenAIImagesRequest(c, req)
	require.NoError(t, err)
	forward, err := gateway.ForwardImages(t.Context(), c, synced, req, parsed, "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, model, forward.UpstreamModel)
	require.Equal(t, 1, imageRequests)
}
