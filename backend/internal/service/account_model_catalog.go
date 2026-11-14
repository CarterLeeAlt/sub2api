package service

import (
	"maps"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const AccountModelCatalogExtraKey = "model_catalog"

// ModelCatalogEdit is an explicit form edit, separate from the server-owned snapshot.
// Revision detects another model-configuration edit, not routine background refreshes.
type ModelCatalogEdit struct {
	Source   string `json:"source"`
	Revision string `json:"revision"`
}

// BulkModelCatalogEdit switches each account to its own upstream catalog.
// Configuration revisions and observations are read under the account row locks.
type BulkModelCatalogEdit struct {
	Source string `json:"source"`
}

func (edit *BulkModelCatalogEdit) Apply(current *Account, credentials, extra map[string]any) (AccountModelCatalog, error) {
	if edit.Source != "upstream" {
		return AccountModelCatalog{}, infraerrors.BadRequest("INVALID_MODEL_CATALOG_SOURCE", "bulk model source must be upstream")
	}
	if _, exists := credentials["model_mapping"]; exists {
		return AccountModelCatalog{}, infraerrors.BadRequest("MODEL_CATALOG_MAPPING_CONFLICT", "bulk upstream catalog preserves each account's model mappings; do not supply model_mapping")
	}
	if current == nil {
		return AccountModelCatalog{}, ErrAccountNotFound
	}
	incoming := *current
	incoming.Credentials = make(map[string]any, len(current.Credentials)+len(credentials))
	maps.Copy(incoming.Credentials, current.Credentials)
	maps.Copy(incoming.Credentials, credentials)
	incoming.Extra = make(map[string]any, len(current.Extra)+len(extra))
	maps.Copy(incoming.Extra, current.Extra)
	maps.Copy(incoming.Extra, extra)
	incoming.ModelCatalogEdit = &ModelCatalogEdit{Source: edit.Source, Revision: current.ModelCatalog().Revision}
	if err := MergeAccountModelCatalog(&incoming, current); err != nil {
		return AccountModelCatalog{}, err
	}
	return incoming.ModelCatalog(), nil
}

type AccountModelCatalog struct {
	Source      string `json:"source"`
	Revision    string `json:"revision"`
	SyncedAt    string `json:"synced_at,omitempty"`
	Ready       bool   `json:"ready"`
	AttemptedAt string `json:"attempted_at,omitempty"`
	NextSyncAt  string `json:"next_sync_at,omitempty"`
	Error       string `json:"error,omitempty"`
	Added       int    `json:"added"`
	Removed     int    `json:"removed"`
}

func (a *Account) SupportsAutomaticModelCatalog() bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth &&
		!a.IsCredentialShadow() && !a.IsOpenAIAgentIdentity() && !a.IsOpenAIPersonalAccessToken() && !a.IsOpenAIPassthroughEnabled()
}

func (a *Account) ModelCatalog() AccountModelCatalog {
	var state AccountModelCatalog
	if a != nil && a.Extra != nil {
		value, _ := a.Extra[AccountModelCatalogExtraKey].(map[string]any)
		state.Source, _ = value["source"].(string)
		state.Revision, _ = value["revision"].(string)
		state.Ready, _ = value["ready"].(bool)
		state.SyncedAt, _ = value["synced_at"].(string)
		state.AttemptedAt, _ = value["attempted_at"].(string)
		state.NextSyncAt, _ = value["next_sync_at"].(string)
		state.Error, _ = value["error"].(string)
		state.Added = parseExtraInt(value["added"])
		state.Removed = parseExtraInt(value["removed"])
	}
	return state
}

func (a *Account) ModelCatalogPending() bool {
	if a == nil {
		return false
	}
	state, _ := a.Extra[AccountModelCatalogExtraKey].(map[string]any)
	return state["source"] == "upstream" && state["ready"] != true && a.SupportsAutomaticModelCatalog()
}

func SetAccountModelCatalog(a *Account, state AccountModelCatalog) {
	a.Extra = maps.Clone(a.Extra)
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	a.Extra[AccountModelCatalogExtraKey] = map[string]any{
		"source": state.Source, "revision": state.Revision, "synced_at": state.SyncedAt, "ready": state.Ready,
		"attempted_at": state.AttemptedAt, "next_sync_at": state.NextSyncAt,
		"error": state.Error, "added": state.Added, "removed": state.Removed,
	}
}

func validateModelCatalogSource(source string) error {
	if source != "upstream" && source != "builtin" && source != "custom" {
		return infraerrors.BadRequest("INVALID_MODEL_CATALOG_SOURCE", "model source must be upstream, builtin or custom")
	}
	return nil
}

// InitializeAccountModelCatalog covers all repository creation paths, including imports.
// Explicit model restrictions are treated as custom; otherwise eligible accounts default upstream.
func InitializeAccountModelCatalog(a *Account) error {
	a.Extra = maps.Clone(a.Extra)
	delete(a.Extra, AccountModelCatalogExtraKey)
	if !a.SupportsAutomaticModelCatalog() {
		if a.ModelCatalogEdit != nil {
			return infraerrors.BadRequest("MODEL_CATALOG_UNSUPPORTED", "automatic model catalog requires a regular OpenAI OAuth parent account")
		}
		return nil
	}
	source := "upstream"
	if len(a.GetModelMapping()) > 0 {
		source = "custom"
	}
	if a.ModelCatalogEdit != nil {
		source = a.ModelCatalogEdit.Source
	}
	if err := validateModelCatalogSource(source); err != nil {
		return err
	}
	SetAccountModelCatalog(a, AccountModelCatalog{Source: source, Revision: uuid.NewString()})
	if source == "upstream" {
		ReplaceAccountModelWhitelist(a, nil)
	}
	return nil
}

// MergeAccountModelCatalog runs while the account row is locked. Forms cannot write
// observations or overwrite the current automatic whitelist with a stale form copy.
func MergeAccountModelCatalog(incoming, current *Account) error {
	state := current.ModelCatalog()
	incoming.Extra = maps.Clone(incoming.Extra)
	delete(incoming.Extra, AccountModelCatalogExtraKey)
	edit := incoming.ModelCatalogEdit
	if edit != nil {
		if !incoming.SupportsAutomaticModelCatalog() {
			return infraerrors.BadRequest("MODEL_CATALOG_UNSUPPORTED", "model source is only supported for regular OpenAI OAuth parent accounts")
		}
		if err := validateModelCatalogSource(edit.Source); err != nil {
			return err
		}
		if state.Revision != edit.Revision {
			return infraerrors.Conflict("MODEL_CATALOG_CHANGED", "model configuration changed; reload the account before saving")
		}
		state = AccountModelCatalog{Source: edit.Source, Revision: uuid.NewString()}
		if edit.Source == "upstream" {
			// Existing accounts retain their last valid restrictions while switching source.
			state.SyncedAt = current.ModelCatalog().SyncedAt
			state.Ready = current.ModelCatalog().Ready || (current.ModelCatalog().Source != "upstream" && len(current.GetModelMapping()) > 0)
			models := make([]string, 0)
			for from, to := range current.GetModelMapping() {
				if from == to {
					models = append(models, from)
				}
			}
			ReplaceAccountModelWhitelist(incoming, models)
		}
	} else if state.Source != "" && incoming.SupportsAutomaticModelCatalog() {
		incoming.Credentials = maps.Clone(incoming.Credentials)
		if incoming.Credentials == nil {
			incoming.Credentials = map[string]any{}
		}
		if value, ok := current.Credentials["model_mapping"]; ok {
			incoming.Credentials["model_mapping"] = value
		} else {
			delete(incoming.Credentials, "model_mapping")
		}
	}
	if state.Source != "" {
		if !incoming.SupportsAutomaticModelCatalog() {
			state.Source = "custom"
			state.Revision = uuid.NewString()
			state.NextSyncAt = ""
		}
		SetAccountModelCatalog(incoming, state)
	}
	return nil
}

// ReplaceAccountModelWhitelist keeps explicit aliases, including wildcard mappings.
func ReplaceAccountModelWhitelist(a *Account, models []string) (added, removed int) {
	previous := a.GetModelMapping()
	next := make(map[string]any, len(models)+len(previous))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			next[model] = model
		}
	}
	for from, to := range previous {
		if from != to {
			next[from] = to
		}
	}
	for from, to := range next {
		if to == from && previous[from] != from {
			added++
		}
	}
	for from, to := range previous {
		if from == to && next[from] != from {
			removed++
		}
	}
	a.Credentials = maps.Clone(a.Credentials)
	if a.Credentials == nil {
		a.Credentials = map[string]any{}
	}
	if len(next) == 0 {
		delete(a.Credentials, "model_mapping")
	} else {
		a.Credentials["model_mapping"] = next
	}
	return
}
