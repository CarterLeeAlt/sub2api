package service

import (
	"maps"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

// BulkModelRestrictionEdit replaces only the selected part of the account model
// configuration. Whitelist edits preserve aliases; alias edits preserve the whitelist.
type BulkModelRestrictionEdit struct {
	Mode     string            `json:"mode"`
	Models   []string          `json:"models,omitempty"`
	Mappings map[string]string `json:"mappings,omitempty"`
}

func (edit *BulkModelRestrictionEdit) Validate() error {
	switch edit.Mode {
	case "whitelist":
		if len(edit.Mappings) != 0 {
			return invalidBulkModelRestriction()
		}
		for _, model := range edit.Models {
			if strings.TrimSpace(model) == "" || strings.Contains(model, "*") {
				return invalidBulkModelRestriction()
			}
		}
	case "mapping":
		if len(edit.Models) != 0 {
			return invalidBulkModelRestriction()
		}
		seen := make(map[string]bool, len(edit.Mappings))
		for from, to := range edit.Mappings {
			from, to = strings.TrimSpace(from), strings.TrimSpace(to)
			if from == "" || to == "" || seen[from] ||
				strings.Contains(strings.TrimSuffix(from, "*"), "*") || strings.Contains(to, "*") {
				return invalidBulkModelRestriction()
			}
			seen[from] = true
		}
	default:
		return invalidBulkModelRestriction()
	}
	return nil
}

func invalidBulkModelRestriction() error {
	return infraerrors.BadRequest("INVALID_MODEL_RESTRICTION", "model names must be nonempty with unique sources; only mapping source names may have one trailing wildcard")
}

func validateBulkModelMappingCredentials(credentials map[string]any) error {
	for _, key := range []string{"model_mapping", "compact_model_mapping"} {
		value, present := credentials[key]
		if !present || value == nil {
			continue
		}
		mapping := make(map[string]string)
		switch values := value.(type) {
		case map[string]string:
			mapping = values
		case map[string]any:
			for from, target := range values {
				to, ok := target.(string)
				if !ok {
					return invalidBulkModelRestriction()
				}
				mapping[from] = to
			}
		default:
			return invalidBulkModelRestriction()
		}
		if err := (&BulkModelRestrictionEdit{Mode: "mapping", Mappings: mapping}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (edit *BulkModelRestrictionEdit) Apply(account *Account) error {
	if err := edit.Validate(); err != nil {
		return err
	}
	if account.IsCredentialShadow() {
		return infraerrors.BadRequest("SPARK_SHADOW_NO_CREDENTIALS", "manage model restrictions on the parent account")
	}
	if edit.Mode == "whitelist" {
		ReplaceAccountModelWhitelist(account, edit.Models)
	} else {
		mapping := make(map[string]any)
		for from, to := range account.GetModelMapping() {
			if from == to {
				mapping[from] = to
			}
		}
		for from, to := range edit.Mappings {
			mapping[strings.TrimSpace(from)] = strings.TrimSpace(to)
		}
		account.Credentials = maps.Clone(account.Credentials)
		if account.Credentials == nil {
			account.Credentials = make(map[string]any)
		}
		account.Credentials["model_mapping"] = mapping
	}
	if account.ModelCatalog().Source != "" || account.SupportsAutomaticModelCatalog() {
		SetAccountModelCatalog(account, AccountModelCatalog{Source: "custom", Revision: uuid.NewString(), Ready: true})
	}
	return nil
}
