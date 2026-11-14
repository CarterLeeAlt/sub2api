//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBulkModelRestrictionPreservesIndependentConfiguration(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "token", "model_mapping": map[string]any{"old": "old", "alias": "target", "gpt-*": "target"},
	}}
	require.NoError(t, (&BulkModelRestrictionEdit{Mode: "whitelist", Models: []string{"new"}}).Apply(account))
	require.Equal(t, map[string]string{"new": "new", "alias": "target", "gpt-*": "target"}, account.GetModelMapping())
	require.Equal(t, "custom", account.ModelCatalog().Source)
	require.NoError(t, (&BulkModelRestrictionEdit{Mode: "mapping", Mappings: map[string]string{"other": "target2"}}).Apply(account))
	require.Equal(t, map[string]string{"new": "new", "other": "target2"}, account.GetModelMapping())
	require.NoError(t, (&BulkModelRestrictionEdit{Mode: "whitelist"}).Apply(account))
	require.Equal(t, map[string]string{"other": "target2"}, account.GetModelMapping())
	require.NoError(t, (&BulkModelRestrictionEdit{Mode: "mapping"}).Apply(account))
	require.Empty(t, account.GetModelMapping())
	require.Equal(t, "token", account.GetCredential("access_token"))
}

func TestBulkModelRestrictionRejectsMalformedEdits(t *testing.T) {
	for _, edit := range []BulkModelRestrictionEdit{
		{Mode: "whitelist", Models: []string{"gpt-*"}},
		{Mode: "whitelist", Models: []string{" "}},
		{Mode: "mapping", Mappings: map[string]string{"alias": ""}},
		{Mode: "mapping", Mappings: map[string]string{"alias": "a", " alias ": "b"}},
		{Mode: "mapping", Mappings: map[string]string{"gpt*bad": "target"}},
		{Mode: "mapping", Mappings: map[string]string{"gpt-*": "gpt-*"}},
	} {
		require.Error(t, edit.Validate())
	}
	require.Error(t, validateBulkModelMappingCredentials(map[string]any{"compact_model_mapping": map[string]any{"a": ""}}))
	require.NoError(t, (&BulkModelRestrictionEdit{Mode: "mapping", Mappings: map[string]string{"gpt-*": "target"}}).Validate())
}

type zeroRowsBulkRepo struct{ accountRepoStubForBulkUpdate }

func (r *zeroRowsBulkRepo) BulkUpdate(context.Context, []int64, AccountBulkUpdate) (int64, error) {
	return 0, nil
}

func TestBulkUpdateAccurateRowsAndClearValues(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	zero := 0
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1, 1}, LoadFactor: &zero})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.NotNil(t, repo.lastBulkUpdate.LoadFactor)
	require.Zero(t, *repo.lastBulkUpdate.LoadFactor)
	result, err = (&adminServiceImpl{accountRepo: &zeroRowsBulkRepo{}}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{999}, LoadFactor: &zero})
	require.NoError(t, err)
	require.Zero(t, result.Success)
	require.Equal(t, []int64{999}, result.FailedIDs)
}

func TestBulkUpdateSeedanceAndAppServerSettings(t *testing.T) {
	ctx := context.Background()
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}}}
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Credentials: map[string]any{
		"openai_capabilities": []any{"chat_completions", "embeddings", "seedance"},
	}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, []string{"chat_completions", "embeddings", "seedance"}, repo.lastBulkUpdate.Credentials["openai_capabilities"])
	for _, parentEnabled := range []bool{false, true} {
		repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_cli_only": parentEnabled}}}}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Extra: map[string]any{"codex_cli_only_allow_app_server": true}})
		if parentEnabled {
			require.NoError(t, err)
		} else {
			requireApplicationErrorReason(t, err, "CODEX_APP_SERVER_PARENT_REQUIRED")
			require.Zero(t, repo.bulkUpdateCalls)
		}
	}
}
