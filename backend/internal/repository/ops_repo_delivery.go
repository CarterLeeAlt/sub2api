package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/opsdelivery"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// Absolute snapshots make retrying a transaction safe even after an ambiguous commit.
func (r *opsRepository) SaveDelivery(ctx context.Context, rows []opsdelivery.Row, buckets []opsdelivery.Bucket, covered []time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if len(rows) > 0 {
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ops_delivery_requests SELECT * FROM jsonb_to_recordset($1) AS d("ID" uuid,"Started" timestamptz,"Ended" timestamptz,"RequestID" text,"Platform" text,"GroupID" bigint,"Model" text,"Outcome" text,"FirstMs" float8,"SpanMs" float8,"Chars" bigint,"Tokens" bigint,"UnknownChars" bigint,"FirstChars" bigint,"FirstTokens" bigint,"Attempts" int,"UpstreamErrors" int,"LimitedAttempts" int,"Incomplete" boolean,"UserID" bigint,"APIKeyID" bigint,"AccountID" bigint,"Stream" boolean)
 ON CONFLICT(id) DO UPDATE SET ended_at=excluded.ended_at,platform=excluded.platform,group_id=excluded.group_id,model=excluded.model,outcome=excluded.outcome,first_ms=excluded.first_ms,span_ms=excluded.span_ms,chars=excluded.chars,tokens=excluded.tokens,unknown_chars=excluded.unknown_chars,first_chars=excluded.first_chars,first_tokens=excluded.first_tokens,attempts=excluded.attempts,upstream_errors=excluded.upstream_errors,limited_attempts=excluded.limited_attempts,incomplete_measurement=excluded.incomplete_measurement,user_id=excluded.user_id,api_key_id=excluded.api_key_id,account_id=excluded.account_id,stream=excluded.stream`, string(data))
		if err != nil {
			return err
		}
	}
	if len(buckets) > 0 {
		data, err := json.Marshal(buckets)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ops_delivery_buckets SELECT * FROM jsonb_to_recordset($1) AS d("ID" uuid,"Bucket" timestamptz,"Chars" bigint,"Tokens" bigint,"UnknownChars" bigint) ON CONFLICT(id,bucket) DO UPDATE SET chars=excluded.chars,tokens=excluded.tokens,unknown_chars=excluded.unknown_chars`, string(data))
		if err != nil {
			return err
		}
	}
	if len(covered) > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO ops_delivery_coverage SELECT unnest($1::timestamptz[]) ON CONFLICT DO NOTHING`, pq.Array(covered))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

const deliveryScope = ` AND ($3='' OR r.platform=$3) AND ($4::bigint IS NULL OR r.group_id=$4) `

func (r *opsRepository) addDeliveryTrend(ctx context.Context, f *service.OpsDashboardFilter, seconds int, points []*service.OpsThroughputTrendPoint) error {
	const query = deliverySeries + ` SELECT date_bin(make_interval(secs=>$5),bucket,timestamptz 'epoch'),avg(n)/5.0,CASE WHEN NOT bool_or(missing) THEN avg(CASE WHEN exact THEN tokens ELSE chars END)/5.0 END,bool_or(exact) FROM totals GROUP BY 1 ORDER BY 1`
	rows, err := r.db.QueryContext(ctx, query, f.StartTime.UTC().Truncate(opsdelivery.Window), f.EndTime.UTC().Truncate(opsdelivery.Window), f.Platform, f.GroupID, seconds)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTime := make(map[int64]*service.OpsThroughputTrendPoint, len(points))
	for _, p := range points {
		byTime[p.BucketStart.Unix()] = p
	}
	for rows.Next() {
		var at time.Time
		var qps float64
		var rate *float64
		var exact bool
		if err := rows.Scan(&at, &qps, &rate, &exact); err != nil {
			return err
		}
		if p := byTime[at.Unix()]; p != nil {
			p.DeliveryQPS = &qps
			p.DeliveryRate = rate
			p.DeliveryUnit = "char"
			if exact {
				p.DeliveryUnit = "token"
			}
		}
	}
	return rows.Err()
}

func (r *opsRepository) DeliveryWindow(ctx context.Context, f *service.OpsDashboardFilter, realtime bool) (*service.DeliveryWindow, error) {
	end := f.EndTime.UTC().Truncate(opsdelivery.Window)
	start := f.StartTime.UTC().Truncate(opsdelivery.Window)
	args := []any{start, end, f.Platform, f.GroupID}
	d := &service.DeliveryWindow{Unit: "char", SpeedUnit: "char"}
	const window = deliverySeries + `
 SELECT count(*)*5,min(bucket),max(bucket)+interval '5 seconds',coalesce(bool_or(exact),false),coalesce(sum(n),0),coalesce(sum(CASE WHEN exact THEN tokens ELSE chars END),0),1.0-sum(unknown)::float8/nullif(sum(chars),0),coalesce(bool_or(missing),false),coalesce(bool_or(bucket=$2::timestamptz-interval '5 seconds'),false),
 coalesce(max(n) FILTER(WHERE bucket=$2::timestamptz-interval '5 seconds'),0)/5.0,coalesce(max(n),0)/5.0,coalesce(avg(n),0)/5.0,
 coalesce(max(CASE WHEN exact THEN tokens ELSE chars END) FILTER(WHERE bucket=$2::timestamptz-interval '5 seconds'),0)/5.0,coalesce(max(CASE WHEN exact THEN tokens ELSE chars END),0)/5.0,coalesce(avg(CASE WHEN exact THEN tokens ELSE chars END),0)/5.0 FROM totals`
	var exact bool
	err := r.db.QueryRowContext(ctx, window, args...).Scan(&d.Seconds, &d.From, &d.To, &exact, &d.Requests, &d.Output, &d.TokenCoverage, &d.Missing, &d.CurrentAvailable, &d.QPS.Current, &d.QPS.Peak, &d.QPS.Avg, &d.Rate.Current, &d.Rate.Peak, &d.Rate.Avg)
	if err != nil {
		return nil, err
	}
	if exact {
		d.Unit = "token"
	}
	if realtime || d.Seconds == 0 {
		return d, nil
	}
	return r.deliverySamples(ctx, args, d)
}

const deliverySeries = `WITH covered AS (SELECT bucket FROM ops_delivery_coverage WHERE bucket >=$1 AND bucket<$2), starts AS (
 SELECT date_bin('5 seconds',started_at,timestamptz 'epoch') bucket,count(*) n FROM ops_delivery_requests r WHERE started_at >=$1 AND started_at<$2 ` + deliveryScope + ` GROUP BY 1), outputs AS (
 SELECT b.bucket,sum(b.chars) chars,sum(b.tokens) tokens,sum(b.unknown_chars) unknown,bool_or(r.incomplete_measurement) missing FROM ops_delivery_buckets b JOIN ops_delivery_requests r ON r.id=b.id WHERE b.bucket >=$1 AND b.bucket<$2 ` + deliveryScope + ` GROUP BY 1), series AS (
 SELECT c.bucket,coalesce(n,0) n,coalesce(chars,0) chars,coalesce(tokens,0) tokens,coalesce(unknown,0) unknown,coalesce(missing,false) missing FROM covered c LEFT JOIN starts USING(bucket) LEFT JOIN outputs USING(bucket)), totals AS (
 SELECT *,sum(unknown) OVER()=0 AND sum(chars) OVER()>0 exact FROM series)`

func (r *opsRepository) deliverySamples(ctx context.Context, args []any, d *service.DeliveryWindow) (*service.DeliveryWindow, error) {
	var err error
	var exact bool
	const samples = `WITH finished AS (SELECT r.*,extract(epoch from ended_at-started_at)*1000 duration FROM ops_delivery_requests r WHERE ended_at >=$1 AND ended_at<$2 ` + deliveryScope + ` AND EXISTS(SELECT 1 FROM ops_delivery_coverage c WHERE c.bucket=date_bin('5 seconds',r.ended_at,timestamptz 'epoch'))), rates AS (
 SELECT *, coalesce(bool_and(unknown_chars=0) FILTER(WHERE span_ms>0 AND NOT incomplete_measurement) OVER(),false) exact FROM finished), metrics AS (SELECT *,CASE WHEN span_ms>0 AND NOT incomplete_measurement THEN (CASE WHEN exact THEN tokens-first_tokens ELSE chars-first_chars END)*1000.0/span_ms END speed FROM rates)
 SELECT count(*) FILTER(WHERE outcome='completed'),count(*) FILTER(WHERE outcome='failed'),count(*) FILTER(WHERE outcome='limited'),count(*) FILTER(WHERE outcome='cancelled'),coalesce(sum(attempts),0),coalesce(sum(upstream_errors),0),coalesce(sum(limited_attempts),0),coalesce(bool_or(incomplete_measurement),false),coalesce(bool_or(exact),false),count(speed),
 percentile_cont(.05) within group(order by speed),percentile_cont(.1) within group(order by speed),percentile_cont(.5) within group(order by speed),avg(speed),
 round(percentile_cont(.5) within group(order by first_ms))::int,round(percentile_cont(.9) within group(order by first_ms))::int,round(percentile_cont(.95) within group(order by first_ms))::int,round(percentile_cont(.99) within group(order by first_ms))::int,round(avg(first_ms))::int,round(max(first_ms))::int,
 round(percentile_cont(.5) within group(order by duration))::int,round(percentile_cont(.9) within group(order by duration))::int,round(percentile_cont(.95) within group(order by duration))::int,round(percentile_cont(.99) within group(order by duration))::int,round(avg(duration))::int,round(max(duration))::int FROM metrics`
	var missing bool
	p := &d.TTFT
	l := &d.Duration
	err = r.db.QueryRowContext(ctx, samples, args...).Scan(&d.Complete, &d.Failed, &d.Limited, &d.Cancelled, &d.Attempts, &d.AttemptErrors, &d.AttemptLimited, &missing, &exact, &d.Speed.SampleCount, &d.Speed.P5, &d.Speed.P10, &d.Speed.P50, &d.Speed.Avg, &p.P50, &p.P90, &p.P95, &p.P99, &p.Avg, &p.Max, &l.P50, &l.P90, &l.P95, &l.P99, &l.Avg, &l.Max)
	if err != nil {
		return nil, err
	}
	if exact {
		d.SpeedUnit = "token"
	}
	d.Missing = d.Missing || missing
	return d, nil
}
