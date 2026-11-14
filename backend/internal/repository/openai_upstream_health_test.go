//go:build unit

package repository

import (
	"context"
	"database/sql"
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

func TestOpenAIUpstreamHealthPostgresCAS(t *testing.T) {
	dsn := os.Getenv("SCOPED_ADMIN_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SCOPED_ADMIN_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	name := fmt.Sprintf("openai_health_%d", time.Now().UnixNano())
	_, err = base.Exec(`CREATE SCHEMA ` + name)
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn+" search_path="+name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(); _, _ = base.Exec(`DROP SCHEMA ` + name + ` CASCADE`); _ = base.Close() })
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	ctx := context.Background()
	require.NoError(t, migrate.Create(ctx, migrate.NewSchema(driver), []*schema.Table{migrate.ProxiesTable, migrate.AccountsTable, migrate.GroupsTable, migrate.AccountGroupsTable}, migrate.WithForeignKeys(false)))
	_, err = db.Exec(`CREATE TABLE scheduler_outbox(id BIGSERIAL PRIMARY KEY,event_type TEXT,account_id BIGINT,group_id BIGINT,payload JSONB,dedup_key TEXT,created_at TIMESTAMPTZ DEFAULT NOW());CREATE UNIQUE INDEX health_outbox_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL`)
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	a := &service.Account{Name: "health", Platform: "openai", Type: "oauth", Status: "active", Schedulable: false, Credentials: map[string]any{"access_token": "old", "refresh_token": "refresh"}}
	require.NoError(t, repo.Create(ctx, a))
	read := func() *service.Account { got, err := repo.GetByID(ctx, a.ID); require.NoError(t, err); return got }
	stale := read()
	state := service.OpenAIUpstreamHealth{Kind: "auth_invalid", Source: "quota", Message: "revoked", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	applied, err := repo.SaveOpenAIUpstreamHealth(ctx, stale, state)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, "error", read().Status)
	require.False(t, read().Schedulable)
	require.NoError(t, repo.Update(ctx, stale))
	require.Equal(t, "error", read().Status)
	require.Contains(t, read().Extra, service.OpenAIUpstreamHealthExtraKey)
	parentID := a.ID
	shadow := &service.Account{Name: "shadow", Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, ParentAccountID: &parentID, Credentials: map[string]any{}}
	require.NoError(t, repo.Create(ctx, shadow))
	shadowView, err := repo.GetByID(ctx, shadow.ID)
	require.NoError(t, err)
	require.Contains(t, shadowView.Extra, service.OpenAIUpstreamHealthExtraKey)
	require.Equal(t, "active", shadowView.Status, "credential failure belongs to parent, not shadow")
	// Ordinary extra writes cannot forge or erase authentication observations.
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.OpenAIUpstreamHealthExtraKey: nil}))
	require.Contains(t, read().Extra, service.OpenAIUpstreamHealthExtraKey)
	// A different operation's success cannot clear a credential failure.
	success := state
	success.Kind = "ok"
	success.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	applied, err = repo.SaveOpenAIUpstreamHealth(ctx, read(), success)
	require.NoError(t, err)
	require.False(t, applied)
	// A newly authorized credential is protected from in-flight failures using the old token.
	require.NoError(t, repo.UpdateCredentials(ctx, a.ID, map[string]any{"access_token": "new", "refresh_token": "new-refresh"}))
	require.NotContains(t, read().Extra, service.OpenAIUpstreamHealthExtraKey)
	require.NoError(t, repo.ClearError(ctx, a.ID))
	state.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	applied, err = repo.SaveOpenAIUpstreamHealth(ctx, stale, state)
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, "active", read().Status)
	state.Kind = "auth_refresh_required"
	applied, err = repo.SaveOpenAIUpstreamHealth(ctx, read(), state)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, "active", read().Status)
	require.NotNil(t, read().TempUnschedulableUntil)
	require.False(t, read().Schedulable)
	// Non-auth failures do not overwrite an outstanding authentication problem.
	state.Kind = "network_error"
	state.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	applied, err = repo.SaveOpenAIUpstreamHealth(ctx, read(), state)
	require.NoError(t, err)
	require.False(t, applied)
	require.NoError(t, repo.UpdateCredentials(ctx, a.ID, map[string]any{"access_token": "refreshed", "refresh_token": "new-refresh"}))
	require.Nil(t, read().TempUnschedulableUntil)
	require.NotContains(t, read().Extra, service.OpenAIUpstreamHealthExtraKey)
	shadowView, err = repo.GetByID(ctx, shadow.ID)
	require.NoError(t, err)
	require.NotContains(t, shadowView.Extra, service.OpenAIUpstreamHealthExtraKey)
}
