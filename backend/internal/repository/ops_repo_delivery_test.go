package repository

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/opsdelivery"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOpsDeliveryPostgres(t *testing.T) {
	dsn := os.Getenv("OPS_DELIVERY_TEST_DSN")
	if dsn == "" {
		t.Skip("requires an isolated PostgreSQL database with the Ops delivery migration")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	repo := &opsRepository{db: db}
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 55, 0, time.UTC)
	second := start.Add(5 * time.Second)
	end := start.Add(6 * time.Second)
	firstMs, spanMs := 2000., 4000.
	id := uuid.NewString()
	group := time.Now().UnixNano()
	row := opsdelivery.Row{ID: id, RequestID: id, Started: start.Add(time.Second), Ended: &end, Platform: "openai", GroupID: group, Model: "test-model", Outcome: "completed", FirstMs: &firstMs, SpanMs: &spanMs, FirstChars: 10, Attempts: 2, UpstreamErrors: 1, Counts: opsdelivery.Counts{Chars: 90, UnknownChars: 90}}
	buckets := []opsdelivery.Bucket{{ID: id, Bucket: start, Counts: opsdelivery.Counts{Chars: 10, UnknownChars: 10}}, {ID: id, Bucket: second, Counts: opsdelivery.Counts{Chars: 80, UnknownChars: 80}}}
	covered := []time.Time{start, second, second.Add(5 * time.Second)}
	for range 2 {
		require.NoError(t, repo.SaveDelivery(ctx, []opsdelivery.Row{row}, buckets, covered))
	}
	f := &service.OpsDashboardFilter{StartTime: start, EndTime: start.Add(15 * time.Second), Platform: "openai", GroupID: &group}
	d, err := repo.DeliveryWindow(ctx, f, false)
	require.NoError(t, err)
	require.EqualValues(t, 15, d.Seconds)
	require.EqualValues(t, 1, d.Requests)
	require.EqualValues(t, 90, d.Output)
	require.Equal(t, "char", d.Unit)
	require.EqualValues(t, 0, d.Rate.Current)
	require.EqualValues(t, 16, d.Rate.Peak)
	require.EqualValues(t, 6, d.Rate.Avg)
	require.EqualValues(t, 20, *d.Speed.P50)
	require.EqualValues(t, 2000, *d.TTFT.P99)
	require.EqualValues(t, 1, d.Complete)
	require.EqualValues(t, 2, d.Attempts)
	require.EqualValues(t, 1, d.AttemptErrors)
	points := fillOpsThroughputBuckets(f.StartTime, f.EndTime, 60, nil)
	require.NoError(t, repo.addDeliveryTrend(ctx, f, 60, points))
	require.EqualValues(t, 2, *points[0].DeliveryRate)
	require.EqualValues(t, 8, *points[1].DeliveryRate)
	stats, err := repo.GetOpenAITokenStats(ctx, &service.OpsOpenAITokenStatsFilter{StartTime: f.StartTime, EndTime: f.EndTime, Platform: f.Platform, GroupID: f.GroupID, TopN: 10})
	require.NoError(t, err)
	require.Len(t, stats.Items, 1)
	require.EqualValues(t, 90, *stats.Items[0].Output)
	require.EqualValues(t, 20, *stats.Items[0].P50)

	t.Run("TTFT details and historical request counts use their respective sources", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `
CREATE TEMP TABLE groups (id bigint,name text,platform text);
CREATE TEMP TABLE accounts (id bigint,platform text);
CREATE TEMP TABLE usage_logs (created_at timestamptz,request_id text,model text,duration_ms int,first_token_ms int,user_id bigint,api_key_id bigint,account_id bigint,group_id bigint,stream bool);
CREATE TEMP TABLE ops_error_logs (id bigint,created_at timestamptz,request_id text,client_request_id text,platform text,model text,duration_ms int,time_to_first_token_ms int,status_code int,error_phase text,severity text,error_message text,user_id bigint,api_key_id bigint,account_id bigint,group_id bigint,stream bool,is_count_tokens bool)`)
		require.NoError(t, err)
		items, total, err := repo.ListRequestDetails(ctx, &service.OpsRequestDetailFilter{StartTime: &f.StartTime, EndTime: &f.EndTime, GroupID: &group, Sort: "ttft_desc"})
		require.NoError(t, err)
		require.EqualValues(t, 1, total)
		require.Equal(t, id, items[0].RequestID)
		require.EqualValues(t, 2000, *items[0].FirstTokenMs)
		require.EqualValues(t, 5000, *items[0].DurationMs)
		_, err = db.ExecContext(ctx, `INSERT INTO groups (id,name,platform) VALUES ($1,'OpenAI','openai')`, group)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO usage_logs (created_at,request_id,group_id,stream) VALUES ($1,$2,$3,true)`, end, id, group)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO ops_error_logs (id,created_at,request_id,platform,group_id,status_code,is_count_tokens,stream) VALUES
 (1,$1,'models-failed','openai',$2,503,false,false), (2,$1,'probe-failed','openai',$2,503,true,false),
 (3,$1,'retry-recovered','openai',$2,200,false,false), (4,$3,'later-error','openai',$2,503,false,false)`, end, group, f.EndTime)
		require.NoError(t, err)
		items, total, err = repo.ListRequestDetails(ctx, &service.OpsRequestDetailFilter{StartTime: &f.StartTime, EndTime: &f.EndTime, GroupID: &group})
		require.NoError(t, err)
		require.EqualValues(t, 2, total)
		require.ElementsMatch(t, []string{id, "models-failed"}, []string{items[0].RequestID, items[1].RequestID})
		groups, err := repo.getThroughputTopGroupsByPlatform(ctx, f.StartTime, f.EndTime, "openai", 100)
		require.NoError(t, err)
		require.Len(t, groups, 1)
		require.EqualValues(t, 2, groups[0].RequestCount)
	})

	t.Run("exact percentiles across hours", func(t *testing.T) {
		longStart := start.Add(-time.Hour)
		longEnd := start.Add(24*time.Hour - time.Hour)
		cover := []time.Time{longStart, longEnd.Add(-5 * time.Second)}
		var samples []opsdelivery.Row
		for i := range 100 {
			at := cover[0]
			ms := 1000.
			if i == 99 {
				at = cover[1]
				ms = 10000
			}
			finished := at.Add(time.Second)
			samples = append(samples, opsdelivery.Row{ID: uuid.NewString(), Started: at, Ended: &finished, Platform: "anthropic", GroupID: group, Outcome: "completed", FirstMs: &ms})
		}
		require.NoError(t, repo.SaveDelivery(ctx, samples, nil, cover))
		result, err := repo.DeliveryWindow(ctx, &service.OpsDashboardFilter{StartTime: longStart, EndTime: longEnd, Platform: "anthropic", GroupID: &group}, false)
		require.NoError(t, err)
		require.EqualValues(t, 1000, *result.TTFT.P95)
		require.EqualValues(t, 1090, *result.TTFT.P99)
		require.EqualValues(t, 10000, *result.TTFT.Max)
	})
}
