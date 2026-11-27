package service

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const officialModelDocsURL = "https://developers.openai.com/api/docs"

type imageModelLifecycle struct {
	Deprecated       map[string]bool   `json:"deprecated"`
	DefaultSnapshots map[string]string `json:"default_snapshots"`
}

func fetchOfficialImageCatalog(ctx context.Context, client *http.Client, specURL, docsURL string, previous imageSpecSnapshot, now time.Time) (imageSpecSnapshot, error) {
	updated, err := fetchOfficialImageSpec(ctx, client, specURL, previous, now)
	if err != nil {
		return previous, err
	}
	lifecycle, err := fetchOfficialImageLifecycle(ctx, client, docsURL, updated.Models)
	if err != nil {
		return previous, err
	}
	if previous.Lifecycle == nil || !maps.Equal(previous.Lifecycle.Deprecated, lifecycle.Deprecated) ||
		!maps.Equal(previous.Lifecycle.DefaultSnapshots, lifecycle.DefaultSnapshots) {
		updated.Updated = now
	}
	updated.Lifecycle = lifecycle
	return updated, nil
}

func fetchOfficialImageLifecycle(ctx context.Context, client *http.Client, docsURL string, models map[string]imageEndpointSupport) (*imageModelLifecycle, error) {
	document, err := fetchOfficialModelMarkdown(ctx, client, docsURL+"/deprecations.md")
	if err != nil {
		return nil, err
	}
	deprecated, err := parseOfficialImageDeprecations(document, models)
	if err != nil {
		return nil, err
	}
	defaults := make(map[string]string)
	for id := range models {
		// A dated suffix only identifies which model page to read. The page's
		// Default snapshot field is the evidence that two IDs are equivalent.
		const datedSuffixLength = len("-2006-01-02")
		if len(id) <= datedSuffixLength || deprecated[id] {
			continue
		}
		alias, suffix := id[:len(id)-datedSuffixLength], id[len(id)-datedSuffixLength:]
		if _, found := models[alias]; !found || deprecated[alias] || !strings.HasPrefix(suffix, "-") {
			continue
		}
		if _, err := time.Parse("2006-01-02", suffix[1:]); err != nil {
			continue
		}
		if _, checked := defaults[alias]; checked {
			continue
		}
		page, err := fetchOfficialModelMarkdown(ctx, client, docsURL+"/models/"+url.PathEscape(alias)+".md")
		if err != nil {
			return nil, err
		}
		match := defaultImageSnapshotLine.FindStringSubmatch(page)
		if len(match) == 0 || !strings.Contains(page, "\nModel ID: `"+alias+"`") {
			return nil, fmt.Errorf("OpenAI model page for %q has no valid default snapshot", alias)
		}
		// Preserve a snapshot with endpoint support the alias cannot replace.
		defaults[alias] = alias
		if models[alias] == models[match[1]] {
			defaults[alias] = match[1]
		}
	}
	return &imageModelLifecycle{Deprecated: deprecated, DefaultSnapshots: defaults}, nil
}

var defaultImageSnapshotLine = regexp.MustCompile("(?m)^- Default snapshot: `([^`\\s]+)`[ \\t\\r]*$")
var deprecationModelToken = regexp.MustCompile(`[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*`)

func parseOfficialImageDeprecations(doc string, models map[string]imageEndpointSupport) (map[string]bool, error) {
	deprecated := make(map[string]bool)
	tables := map[string]int{"## Upcoming deprecations": -1, "## Past deprecations": -1}
	section := ""
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			section = line
			continue
		}
		rows, tracked := tables[section]
		if !tracked || !strings.HasPrefix(line, "|") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 4 {
			continue
		}
		date := strings.TrimSpace(fields[1])
		if date == "Shutdown date" && strings.HasPrefix(strings.TrimSpace(fields[2]), "Affected model") {
			tables[section] = 0
			continue
		}
		if rows < 0 || !strings.ContainsAny(date, "0123456789") {
			continue
		}
		tables[section]++
		for _, id := range deprecationModelToken.FindAllString(fields[2], -1) {
			if isOfficialImageModelID(id, models) {
				deprecated[id] = true
			}
		}
	}
	for section, rows := range tables {
		if rows <= 0 {
			return nil, fmt.Errorf("OpenAI deprecations page has no %q table rows", strings.TrimPrefix(section, "## "))
		}
	}
	return deprecated, nil
}

func isOfficialImageModelID(id string, models map[string]imageEndpointSupport) bool {
	_, listed := models[id]
	return listed || IsGPTImageGenerationModel(id) || id == "chatgpt-image-latest"
}

func fetchOfficialModelMarkdown(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenAI model documentation %s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if len(body) > limit {
		return "", fmt.Errorf("OpenAI model documentation %s is too large", req.URL.Path)
	}
	return string(body), nil
}

func (l *imageModelLifecycle) filterModelIDs(ids []string) []string {
	kept := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !l.Deprecated[id] {
			kept[id] = true
		}
	}
	for alias, snapshot := range l.DefaultSnapshots {
		if alias != snapshot && kept[alias] {
			delete(kept, snapshot)
		}
	}
	models := make([]string, 0, len(kept))
	for id := range kept {
		models = append(models, id)
	}
	return dedupeAndSortModelIDs(models)
}
