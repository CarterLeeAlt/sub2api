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
	due, err := repo.ListDueModelCatalogAccounts(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	observation := read()
	credentials, err := json.Marshal(observation.Credentials)
	require.NoError(t, err)
	state := observation.ModelCatalog()
	state.Ready = true
	state.SyncedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.AttemptedAt = state.SyncedAt
	state.NextSyncAt = time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	service.ReplaceAccountModelWhitelist(observation, []string{"fresh", "alias"})
	applied, err := repo.SaveModelCatalogObservation(ctx, observation, state.Revision, string(credentials), state)
	require.NoError(t, err)
	require.True(t, applied)
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
}
