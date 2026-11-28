//go:build unit

package repository

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogSettlementAfterFailedDebit(t *testing.T) {
	dsn := os.Getenv("SCOPED_ADMIN_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated SCOPED_ADMIN_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer base.Close()
	name := fmt.Sprintf("receipt_retry_%d", time.Now().UnixNano())
	_, err = base.Exec("CREATE SCHEMA " + name)
	require.NoError(t, err)
	defer base.Exec("DROP SCHEMA " + name + " CASCADE")
	db, err := sql.Open("postgres", dsn+" search_path="+name)
	require.NoError(t, err)
	defer db.Close()
	columns := strings.Split(strings.TrimPrefix(usageLogSelectColumns, "id, "), ", ")
	for i := range columns {
		columns[i] += " " + usageLogInsertArgTypes[i]
	}
	_, err = db.Exec(`CREATE TABLE users(id bigint PRIMARY KEY, balance numeric(20,8), deleted_at timestamptz, updated_at timestamptz);
		CREATE TABLE usage_billing_dedup(id bigserial PRIMARY KEY, request_id text,api_key_id bigint,request_fingerprint text, UNIQUE(request_id,api_key_id));
		CREATE TABLE usage_billing_dedup_archive(LIKE usage_billing_dedup);
		CREATE TABLE usage_logs(id bigserial PRIMARY KEY, ` + strings.Join(columns, ", ") + `, UNIQUE(request_id,api_key_id))`)
	require.NoError(t, err)
	logs := newUsageLogRepositoryWithSQL(nil, db)
	billing := NewUsageBillingRepository(nil, db)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	gateway := service.NewOpenAIGatewayService(nil, logs, billing, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	unitPrice := 1.25
	group := &service.Group{ID: 1, Platform: service.PlatformOpenAI, RateMultiplier: 1, WebSearchPricePerCall: &unitPrice}
	key := &service.APIKey{ID: 2, Group: group, GroupID: &group.ID, User: &service.User{ID: 1}}
	record := func() error {
		return gateway.RecordUsage(t.Context(), &service.OpenAIRecordUsageInput{
			Result: &service.OpenAIForwardResult{RequestID: "retry", Model: "alpha-search", WebSearchCalls: 1},
			APIKey: key, User: key.User, Account: &service.Account{ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		})
	}
	require.ErrorIs(t, record(), service.ErrUserNotFound)
	var before float64
	require.NoError(t, db.QueryRow(`SELECT actual_cost FROM usage_logs WHERE request_id='retry'`).Scan(&before))
	require.Zero(t, before)
	_, err = db.Exec(`INSERT INTO users(id,balance) VALUES (1,100)`)
	require.NoError(t, err)
	const concurrentRetries = 4
	errors := make(chan error, concurrentRetries)
	for range concurrentRetries {
		go func() { errors <- record() }()
	}
	for range concurrentRetries {
		require.NoError(t, <-errors)
	}
	// A delayed failure receipt and an ordinary duplicate cannot replace the payment.
	created, err := logs.Create(t.Context(), &service.UsageLog{RequestID: "retry", APIKeyID: key.ID})
	require.NoError(t, err)
	require.False(t, created)
	var count int
	var balance, actual float64
	err = db.QueryRow(`SELECT (SELECT balance FROM users WHERE id=1), count(*), sum(actual_cost) FROM usage_logs`).Scan(&balance, &count, &actual)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, 98.75, balance)
	require.Equal(t, 1.25, actual)
}
