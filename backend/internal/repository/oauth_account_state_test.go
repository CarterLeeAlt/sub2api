//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
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

func oauthStateTestRepository(t *testing.T) (*accountRepository, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SCOPED_ADMIN_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SCOPED_ADMIN_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	name := fmt.Sprintf("oauth_state_%d", time.Now().UnixNano())
	_, err = base.Exec(`CREATE SCHEMA ` + name)
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn+" search_path="+name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(); _, _ = base.Exec(`DROP SCHEMA ` + name + ` CASCADE`); _ = base.Close() })
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	require.NoError(t, migrate.Create(context.Background(), migrate.NewSchema(driver), []*schema.Table{migrate.ProxiesTable, migrate.AccountsTable, migrate.GroupsTable, migrate.AccountGroupsTable}, migrate.WithForeignKeys(false)))
	_, err = db.Exec(`CREATE TABLE scheduler_outbox(id BIGSERIAL PRIMARY KEY,event_type TEXT,account_id BIGINT,group_id BIGINT,payload JSONB,dedup_key TEXT,created_at TIMESTAMPTZ DEFAULT NOW());CREATE UNIQUE INDEX oauth_outbox_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL`)
	require.NoError(t, err)
	return newAccountRepositoryWithSQL(client, db, nil), db
}

func TestOAuthStatePostgresRotationAndRecovery(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			repo, db := oauthStateTestRepository(t)
			ctx := context.Background()
			a := &service.Account{Name: platform, Platform: platform, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old", "refresh_token": "refresh"}}
			require.NoError(t, repo.Create(ctx, a))
			read := func() *service.Account { got, err := repo.GetByID(ctx, a.ID); require.NoError(t, err); return got }
			old := read()
			state := service.OpenAIUpstreamHealth{Kind: "auth_refresh_required", Source: "gateway", Message: "expired", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			applied, err := repo.SaveOAuthAuthenticationState(ctx, old, state, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.True(t, applied)
			require.NotNil(t, read().TempUnschedulableUntil)
			// Non-identity edits must survive a concurrent successful token rotation.
			_, err = db.Exec(`UPDATE accounts SET credentials=credentials || '{"model_mapping":{"alias":"latest"}}'::jsonb WHERE id=$1`, a.ID)
			require.NoError(t, err)
			rotated := maps.Clone(old.Credentials)
			rotated["access_token"], rotated["refresh_token"] = "new", "new-refresh"
			if platform == service.PlatformOpenAI {
				applied, err = repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, old.ID, old.Credentials, old.ProxyID, rotated)
			} else {
				applied, err = repo.UpdateOAuthCredentialsIfUnchanged(ctx, old, rotated)
			}
			require.NoError(t, err)
			require.True(t, applied)
			require.Nil(t, read().TempUnschedulableUntil)
			require.Empty(t, read().TempUnschedulableReason)
			require.NotContains(t, read().Extra, service.OpenAIUpstreamHealthExtraKey)
			require.Equal(t, "new", read().GetCredential("access_token"))
			require.Equal(t, "latest", read().GetMappedModel("alias"))
			state.Kind = "auth_invalid"
			applied, err = repo.SaveOAuthAuthenticationState(ctx, old, state, time.Time{})
			require.NoError(t, err)
			require.False(t, applied, "late failure cannot disable rotated credentials")
			require.Equal(t, service.StatusActive, read().Status)
		})
	}
}

func TestOAuthStatePostgresConcurrentReauthorization(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		for _, operation := range []string{"rotate", "reject", "reset-recovery"} {
			if operation == "reset-recovery" && platform != service.PlatformAnthropic {
				continue
			}
			t.Run(platform+"/"+operation, func(t *testing.T) {
				repo, db := oauthStateTestRepository(t)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				a := &service.Account{Name: "concurrent", Platform: platform, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "old", "refresh_token": "refresh"}}
				require.NoError(t, repo.Create(ctx, a))
				if operation == "reset-recovery" {
					require.NoError(t, repo.SetRateLimited(ctx, a.ID, time.Now().Add(time.Hour)))
				}
				old, err := repo.GetByID(ctx, a.ID)
				require.NoError(t, err)
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer tx.Rollback()
				_, err = tx.ExecContext(ctx, `UPDATE accounts SET credentials=credentials || '{"access_token":"reauthorized","refresh_token":"reauthorized-refresh"}'::jsonb WHERE id=$1`, a.ID)
				require.NoError(t, err)
				type result struct {
					applied bool
					err     error
				}
				done := make(chan result, 1)
				go func() {
					var applied bool
					var err error
					if operation == "rotate" {
						if platform == service.PlatformOpenAI {
							applied, err = repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, old.ID, old.Credentials, old.ProxyID, map[string]any{"access_token": "late", "refresh_token": "late-refresh"})
						} else {
							applied, err = repo.UpdateOAuthCredentialsIfUnchanged(ctx, old, map[string]any{"access_token": "late", "refresh_token": "late-refresh"})
						}
					} else if operation == "reset-recovery" {
						applied, err = repo.RecoverClaudeReset(ctx, old, service.ClaudeResetRecovery{ClearGlobal: true, Cleared: []string{"five_hour", "seven_day"}})
					} else {
						applied, err = repo.SaveOAuthAuthenticationState(ctx, old, service.OpenAIUpstreamHealth{Kind: "auth_invalid", Message: "late rejection"}, time.Time{})
					}
					done <- result{applied, err}
				}()
				// Observe the competing UPDATE waiting on the real PostgreSQL row lock.
				require.Eventually(t, func() bool {
					var waiting bool
					err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'WITH changed AS (%')`).Scan(&waiting)
					return err == nil && waiting
				}, 3*time.Second, 10*time.Millisecond)
				require.NoError(t, tx.Commit())
				got := <-done
				require.NoError(t, got.err)
				require.False(t, got.applied)
				current, err := repo.GetByID(ctx, a.ID)
				require.NoError(t, err)
				require.Equal(t, "reauthorized", current.GetCredential("access_token"))
				require.Equal(t, service.StatusActive, current.Status)
				if operation == "reset-recovery" {
					require.NotNil(t, current.RateLimitResetAt)
				}
			})
		}
	}
}

func TestOAuthStatePostgresIdentityFieldPresence(t *testing.T) {
	repo, db := oauthStateTestRepository(t)
	ctx := context.Background()
	for _, mutation := range []string{
		`credentials || '{"organization_id":"new-org"}'::jsonb`,
		`credentials - 'access_token'`,
	} {
		a := &service.Account{Name: "presence", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "old", "refresh_token": "refresh"}}
		require.NoError(t, repo.Create(ctx, a))
		old, err := repo.GetByID(ctx, a.ID)
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE accounts SET credentials=`+mutation+` WHERE id=$1`, a.ID)
		require.NoError(t, err)
		applied, err := repo.SaveOAuthAuthenticationState(ctx, old, service.OpenAIUpstreamHealth{Kind: "auth_invalid"}, time.Time{})
		require.NoError(t, err)
		require.False(t, applied, "adding or removing an identity field invalidates the snapshot")
	}
}

func TestClaudeResetPostgresRecovery(t *testing.T) {
	repo, _ := oauthStateTestRepository(t)
	ctx := context.Background()
	a := &service.Account{Name: "reset", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "token", "refresh_token": "refresh"}, Extra: map[string]any{"session_window_utilization": 1.0, "passive_usage_7d_utilization": 1.0}}
	require.NoError(t, repo.Create(ctx, a))
	require.NoError(t, repo.SetRateLimited(ctx, a.ID, time.Now().Add(time.Hour)))
	limited, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	recovery := service.ClaudeResetRecovery{ClearGlobal: true, Cleared: []string{"five_hour", "seven_day"}}
	applied, err := repo.RecoverClaudeReset(ctx, limited, recovery)
	require.NoError(t, err)
	require.True(t, applied)
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, current.RateLimitResetAt)
	require.Equal(t, float64(0), current.Extra["session_window_utilization"])
	require.NoError(t, repo.SetRateLimited(ctx, a.ID, time.Now().Add(2*time.Hour)))
	applied, err = repo.RecoverClaudeReset(ctx, limited, recovery)
	require.NoError(t, err)
	require.False(t, applied, "late reset recovery cannot erase a newer limit")
}
