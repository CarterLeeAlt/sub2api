//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestModelCatalogPostgresConsistency(t *testing.T) {
	dsn := os.Getenv("SCOPED_ADMIN_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SCOPED_ADMIN_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	name := fmt.Sprintf("model_catalog_%d", time.Now().UnixNano())
	_, err = base.Exec(`CREATE SCHEMA ` + name)
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn+" search_path="+name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(); _, _ = base.Exec(`DROP SCHEMA ` + name + ` CASCADE`); _ = base.Close() })
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	ctx := context.Background()
	require.NoError(t, migrate.Create(ctx, migrate.NewSchema(driver), []*schema.Table{migrate.ProxiesTable, migrate.AccountsTable, migrate.GroupsTable, migrate.AccountGroupsTable}, migrate.WithForeignKeys(false)))
	_, err = db.Exec(`CREATE TABLE scheduler_outbox(id BIGSERIAL PRIMARY KEY,event_type TEXT,account_id BIGINT,group_id BIGINT,payload JSONB,dedup_key TEXT,created_at TIMESTAMPTZ DEFAULT NOW());CREATE UNIQUE INDEX catalog_outbox_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL`)
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	wakes := 0
	repo.SetModelCatalogWake(func() { wakes++ })
	a := &service.Account{Name: "catalog", Platform: "openai", Type: "oauth", Status: "active", Schedulable: false, Credentials: map[string]any{"access_token": "token", "model_mapping": map[string]any{"alias": "manual"}}, ModelCatalogEdit: &service.ModelCatalogEdit{Source: "upstream"}}
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.Create(dbent.NewTxContext(ctx, tx), a))
	require.Zero(t, wakes)
	require.NoError(t, tx.Commit())
	require.Equal(t, 1, wakes)
	require.True(t, a.ModelCatalogPending())
	read := func() *service.Account { got, err := repo.GetByID(ctx, a.ID); require.NoError(t, err); return got }
	stale := read()
	due, err := repo.ListDueModelCatalogAccounts(ctx, 0, 100, false)
	require.NoError(t, err)
	require.Len(t, due, 1)
	observation := read()
	credentials, err := json.Marshal(observation.Credentials)
	require.NoError(t, err)
	state := observation.ModelCatalog()
	state.Ready = true
	state.PendingImageManifest = `[{"slug":"fresh"}]`
	state.SyncedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.AttemptedAt = state.SyncedAt
	state.NextSyncAt = time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	service.ReplaceAccountModelWhitelist(observation, []string{"fresh", "alias"})
	applied, err := repo.SaveModelCatalogObservation(ctx, observation, state.Revision, string(credentials), state)
	require.NoError(t, err)
	require.True(t, applied)
	due, err = repo.ListDueModelCatalogAccounts(ctx, 0, 100, false)
	require.NoError(t, err)
	require.Empty(t, due)
	due, err = repo.ListDueModelCatalogAccounts(ctx, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, state.PendingImageManifest, due[0].ModelCatalog().PendingImageManifest)
	// Replayed observations cannot overwrite a newer observation, even with the same configuration revision.
	applied, err = repo.SaveModelCatalogObservation(ctx, observation, state.Revision, string(credentials), state)
	require.NoError(t, err)
	require.False(t, applied)
	require.False(t, read().Schedulable)
	require.False(t, read().ModelCatalogPending())
	require.NoError(t, repo.Update(ctx, stale))
	require.Equal(t, map[string]string{"fresh": "fresh", "alias": "manual"}, read().GetModelMapping())
	stale.Credentials["access_token"] = "refreshed"
	require.NoError(t, repo.UpdateCredentials(ctx, a.ID, stale.Credentials))
	require.Equal(t, map[string]string{"fresh": "fresh", "alias": "manual"}, read().GetModelMapping())
	require.Equal(t, "refreshed", read().GetCredential("access_token"))
	beforeEdit := read()
	beforeJSON, err := json.Marshal(beforeEdit.Credentials)
	require.NoError(t, err)
	edit := read()
	edit.ModelCatalogEdit = &service.ModelCatalogEdit{Source: "custom", Revision: edit.ModelCatalog().Revision}
	edit.Credentials["model_mapping"] = map[string]any{"alias": "manual"}
	require.NoError(t, repo.Update(ctx, edit))
	applied, err = repo.SaveModelCatalogObservation(ctx, beforeEdit, beforeEdit.ModelCatalog().Revision, string(beforeJSON), state)
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, map[string]string{"alias": "manual"}, read().GetModelMapping())
	_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"model_mapping": map[string]any{"bulk": "bulk"}}, Extra: map[string]any{"model_catalog": map[string]any{"source": "upstream", "ready": false}}})
	require.NoError(t, err)
	require.Equal(t, "custom", read().ModelCatalog().Source)
	require.Equal(t, map[string]string{"bulk": "bulk"}, read().GetModelMapping())
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{"model_catalog": map[string]any{"source": "upstream"}}))
	require.Equal(t, "custom", read().ModelCatalog().Source)
	edit = read()
	edit.ModelCatalogEdit = &service.ModelCatalogEdit{Source: "upstream", Revision: edit.ModelCatalog().Revision}
	require.NoError(t, repo.Update(ctx, edit))
	require.True(t, read().ModelCatalog().Ready)
	require.NoError(t, repo.FinishModelCatalogRound(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	// A source edit starts a new round and cannot be delayed by the previous round's completion.
	require.Empty(t, read().ModelCatalog().NextSyncAt)

	t.Run("round completion preserves failed observation retry deadline", func(t *testing.T) {
		observation := read()
		credentials, err := json.Marshal(observation.Credentials)
		require.NoError(t, err)
		state := observation.ModelCatalog()
		state.Error = "temporary upstream failure"
		state.AttemptedAt = time.Now().UTC().Format(time.RFC3339Nano)
		state.NextSyncAt = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
		applied, err := repo.SaveModelCatalogObservation(ctx, observation, state.Revision, string(credentials), state)
		require.NoError(t, err)
		require.True(t, applied)
		next := time.Now().Add(time.Hour)
		require.NoError(t, repo.FinishModelCatalogRound(ctx, time.Now().Add(-time.Minute), next))
		require.Equal(t, state.NextSyncAt, read().ModelCatalog().NextSyncAt)
		observation = read()
		state.Error = ""
		state.AttemptedAt = time.Now().UTC().Format(time.RFC3339Nano)
		applied, err = repo.SaveModelCatalogObservation(ctx, observation, state.Revision, string(credentials), state)
		require.NoError(t, err)
		require.True(t, applied)
		require.NoError(t, repo.FinishModelCatalogRound(ctx, time.Now().Add(-time.Minute), next))
		require.Equal(t, next.Format(time.RFC3339Nano), read().ModelCatalog().NextSyncAt)
	})

	t.Run("compound bulk edits roll back fields groups and shadows", func(t *testing.T) {
		before := read()
		priority := before.Priority + 10
		invalidGroups := []int64{99999999}
		_, err := repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Priority: &priority, GroupIDs: &invalidGroups})
		require.Error(t, err)
		require.Equal(t, before.Priority, read().Priority)
		shadow := &service.Account{Name: "bulk-shadow", Platform: "openai", Type: "oauth", Status: "active", ParentAccountID: &a.ID, QuotaDimension: "spark", Credentials: map[string]any{}}
		require.NoError(t, repo.Create(ctx, shadow))
		_, err = db.Exec(`CREATE FUNCTION reject_shadow_proxy() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.parent_account_id IS NOT NULL THEN RAISE EXCEPTION 'shadow write rejected'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER reject_shadow_proxy BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION reject_shadow_proxy()`)
		require.NoError(t, err)
		clearProxy := int64(0)
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Priority: &priority, ProxyID: &clearProxy, PropagateProxy: true})
		require.ErrorContains(t, err, "shadow write rejected")
		require.Equal(t, before.Priority, read().Priority)
		_, err = db.Exec(`DROP TRIGGER reject_shadow_proxy ON accounts`)
		require.NoError(t, err)
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Priority: &priority, ProxyID: &clearProxy, PropagateProxy: true})
		require.NoError(t, err)
		require.Equal(t, priority, read().Priority)
		storedShadow, err := repo.GetByID(ctx, shadow.ID)
		require.NoError(t, err)
		require.Nil(t, storedShadow.ProxyID)
		factor := 20
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{LoadFactor: &factor})
		require.NoError(t, err)
		factor = 0
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{LoadFactor: &factor})
		require.NoError(t, err)
		require.Nil(t, read().LoadFactor)
	})

	t.Run("bulk model parts preserve aliases and whitelist independently", func(t *testing.T) {
		_, err := repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{Credentials: map[string]any{"model_mapping": map[string]any{"old": "old", "alias": "target"}}})
		require.NoError(t, err)
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{ModelRestriction: &service.BulkModelRestrictionEdit{Mode: "whitelist", Models: []string{"new"}}})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"new": "new", "alias": "target"}, read().GetModelMapping())
		_, err = repo.BulkUpdate(ctx, []int64{a.ID}, service.AccountBulkUpdate{ModelRestriction: &service.BulkModelRestrictionEdit{Mode: "mapping"}})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"new": "new"}, read().GetModelMapping())
		require.Equal(t, "custom", read().ModelCatalog().Source)
	})

	t.Run("bulk upstream preserves per-account models and commits atomically", func(t *testing.T) {
		second := &service.Account{Name: "second", Platform: "openai", Type: "oauth", Status: "active", Credentials: map[string]any{
			"auth_mode":    service.OpenAIAuthModePersonalAccessToken,
			"access_token": "at-second-token", "model_mapping": map[string]any{"second-model": "second-model", "second-alias": "second-target"},
		}}
		require.NoError(t, repo.Create(ctx, second))
		beforeFirst := read()
		beforeSecond, err := repo.GetByID(ctx, second.ID)
		require.NoError(t, err)
		updates := service.AccountBulkUpdate{ModelCatalog: &service.BulkModelCatalogEdit{Source: "upstream"}}

		// A missing or unsupported target prevents all writes, including other fields.
		newPriority := 42
		invalid := &service.Account{Name: "api-key", Platform: "openai", Type: "apikey", Status: "active", Credentials: map[string]any{"api_key": "test"}}
		require.NoError(t, repo.Create(ctx, invalid))
		for _, id := range []int64{invalid.ID, invalid.ID + 1000} {
			_, err = repo.BulkUpdate(ctx, []int64{a.ID, id}, service.AccountBulkUpdate{ModelCatalog: updates.ModelCatalog, Priority: &newPriority})
			require.Error(t, err)
			require.Equal(t, beforeFirst.Priority, read().Priority)
			require.Equal(t, beforeFirst.ModelCatalog(), read().ModelCatalog())
		}
		// Supplied credentials/extra are included in eligibility checks under lock.
		_, err = repo.BulkUpdate(ctx, []int64{a.ID, second.ID}, service.AccountBulkUpdate{ModelCatalog: updates.ModelCatalog, Extra: map[string]any{"openai_passthrough": true}})
		require.Error(t, err)
		require.Equal(t, beforeFirst.ModelCatalog(), read().ModelCatalog())

		wakesBefore := wakes
		tx, err := client.Tx(ctx)
		require.NoError(t, err)
		_, err = repo.BulkUpdate(dbent.NewTxContext(ctx, tx), []int64{a.ID, second.ID}, updates)
		require.NoError(t, err)
		require.Equal(t, wakesBefore, wakes)
		require.NoError(t, tx.Rollback())
		require.Equal(t, beforeFirst.ModelCatalog(), read().ModelCatalog())
		require.Equal(t, wakesBefore, wakes)

		rows, err := repo.BulkUpdate(ctx, []int64{second.ID, a.ID, second.ID}, updates)
		require.NoError(t, err)
		require.EqualValues(t, 2, rows)
		require.Equal(t, wakesBefore+1, wakes)
		first := read()
		second, err = repo.GetByID(ctx, second.ID)
		require.NoError(t, err)
		require.Equal(t, beforeFirst.Credentials, first.Credentials)
		require.Equal(t, beforeSecond.Credentials, second.Credentials)
		require.Equal(t, "upstream", second.ModelCatalog().Source)
		require.True(t, second.ModelCatalog().Ready)
		require.Empty(t, second.ModelCatalog().NextSyncAt)
		require.NotEqual(t, beforeFirst.ModelCatalog().Revision, first.ModelCatalog().Revision)
		require.NotEqual(t, first.ModelCatalog().Revision, second.ModelCatalog().Revision)
		// An in-flight observation from before the bulk edit cannot undo it.
		previousCredentials, err := json.Marshal(beforeFirst.Credentials)
		require.NoError(t, err)
		applied, err := repo.SaveModelCatalogObservation(ctx, beforeFirst, beforeFirst.ModelCatalog().Revision, string(previousCredentials), beforeFirst.ModelCatalog())
		require.NoError(t, err)
		require.False(t, applied)
		// Each subsequent upstream result stays local to its account and keeps aliases.
		for i, target := range []*service.Account{first, second} {
			previousCredentials, err := json.Marshal(target.Credentials)
			require.NoError(t, err)
			state := target.ModelCatalog()
			state.Ready = true
			state.AttemptedAt = time.Now().UTC().Format(time.RFC3339Nano)
			state.SyncedAt = state.AttemptedAt
			model := fmt.Sprintf("upstream-%d", i)
			service.ReplaceAccountModelWhitelist(target, []string{model})
			applied, err := repo.SaveModelCatalogObservation(ctx, target, state.Revision, string(previousCredentials), state)
			require.NoError(t, err)
			require.True(t, applied)
			stored, err := repo.GetByID(ctx, target.ID)
			require.NoError(t, err)
			require.Equal(t, target.GetModelMapping(), stored.GetModelMapping())
			require.Equal(t, model, stored.GetModelMapping()[model])
			if i == 1 {
				require.Equal(t, "second-target", stored.GetModelMapping()["second-alias"])
			}
		}
	})
}
