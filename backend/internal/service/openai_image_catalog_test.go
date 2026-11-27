package service

import (
	"encoding/json"
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

func TestOfficialImageCatalogSharedHourlyCheck(t *testing.T) {
	previous := officialImageModels.Swap(nil)
	defer officialImageModels.Store(previous)
	now := time.Now().UTC()
	cache, err := json.Marshal(imageSpecSnapshot{Models: map[string]imageEndpointSupport{"fixture-painter": {Generate: true}}, Checked: now.Add(-59 * time.Minute), Updated: now.Add(-24 * time.Hour)})
	require.NoError(t, err)
	settings := newCodexVersionSyncSettingRepoStub(map[string]string{imageSpecSetting: string(cache)})
	for _, leader := range []bool{false, true, true} {
		locks := &fakeLeaderLockCache{}
		if !leader {
			_, _ = locks.TryAcquireLeaderLock(t.Context(), "openai:image-catalog:refresh", "other", time.Minute)
		}
		worker := OpenAIModelCatalogRefreshService{settings: settings, locks: locks, owner: "local"}
		require.NoError(t, worker.refreshImageSpec(t.Context()))
		require.Empty(t, settings.syncedWrites())
		require.Zero(t, worker.nextImageSpecFetch)
		require.True(t, officialImageModels.Load().Models["fixture-painter"].Generate)
	}
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

func TestOAuthSyncListsOfficialImageModelsWithoutGeneratingImages(t *testing.T) {
	models, err := parseOfficialImageModels([]byte(fixtureImageSpec))
	require.NoError(t, err)
	models["withheld-painter"] = imageEndpointSupport{Generate: true}
	models["editing-only"] = imageEndpointSupport{Edit: true}
	previous := officialImageModels.Swap(&imageSpecSnapshot{Models: models, Updated: time.Now()})
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
