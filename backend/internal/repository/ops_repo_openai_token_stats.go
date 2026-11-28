package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/opsdelivery"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *opsRepository) GetOpenAITokenStats(ctx context.Context, filter *service.OpsOpenAITokenStatsFilter) (*service.OpsOpenAITokenStatsResponse, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil {
		return nil, fmt.Errorf("nil filter")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, fmt.Errorf("start_time/end_time required")
	}
	// 允许 start_time == end_time（结果为空），与 service 层校验口径保持一致。
	if filter.StartTime.After(filter.EndTime) {
		return nil, fmt.Errorf("start_time must be <= end_time")
	}

	dashboardFilter := &service.OpsDashboardFilter{
		StartTime: filter.StartTime.UTC(),
		EndTime:   filter.EndTime.UTC(),
		Platform:  strings.TrimSpace(strings.ToLower(filter.Platform)),
		GroupID:   filter.GroupID,
	}

	baseArgs := []any{dashboardFilter.StartTime.Truncate(opsdelivery.Window), dashboardFilter.EndTime.Truncate(opsdelivery.Window), dashboardFilter.Platform, dashboardFilter.GroupID}
	next := 5

	baseCTE := `
WITH covered AS (SELECT bucket FROM ops_delivery_coverage WHERE bucket >= $1 AND bucket < $2), events AS (
 SELECT r.model,1 n,0 chars,0 tokens,0 unknown,NULL::float8 first_ms,NULL::float8 duration,NULL::float8 char_rate,NULL::float8 token_rate,false missing
 FROM ops_delivery_requests r JOIN covered ON bucket=date_bin('5 seconds',started_at,timestamptz 'epoch') WHERE started_at >= $1 AND started_at < $2 ` + deliveryScope + `
 UNION ALL
 SELECT r.model,0,b.chars,b.tokens,b.unknown_chars,NULL,NULL,NULL,NULL,r.incomplete_measurement
 FROM ops_delivery_buckets b JOIN covered USING(bucket) JOIN ops_delivery_requests r ON r.id=b.id WHERE b.bucket >= $1 AND b.bucket < $2 ` + deliveryScope + `
 UNION ALL
 SELECT r.model,0,0,0,0,first_ms,extract(epoch FROM ended_at-started_at)*1000,(chars-first_chars)*1000.0/nullif(span_ms,0),CASE WHEN unknown_chars=0 THEN (tokens-first_tokens)*1000.0/nullif(span_ms,0) END,r.incomplete_measurement
 FROM ops_delivery_requests r JOIN covered ON bucket=date_bin('5 seconds',ended_at,timestamptz 'epoch') WHERE ended_at >= $1 AND ended_at < $2 ` + deliveryScope + `
), stats AS (
 SELECT model,sum(n) request_count,CASE WHEN NOT bool_or(missing) THEN percentile_cont(.5) within group(order by token_rate) END token_p50,
 CASE WHEN NOT bool_or(missing) THEN percentile_cont(.5) within group(order by char_rate) END char_p50, count(token_rate)=count(char_rate) exact_speed,
 avg(first_ms) avg_first_token_ms,CASE WHEN NOT bool_or(missing) THEN sum(tokens) END output_tokens,
 CASE WHEN NOT bool_or(missing) THEN sum(chars) END output_chars,sum(unknown)=0 AND sum(chars)>0 exact_output,
 coalesce(round(avg(duration)),0)::bigint avg_duration_ms,count(first_ms) requests_with_first_token FROM events GROUP BY model)
`

	countSQL := baseCTE + `SELECT COUNT(*) FROM stats`
	var total int64
	if err := r.db.QueryRowContext(ctx, countSQL, baseArgs...).Scan(&total); err != nil {
		return nil, err
	}

	querySQL := baseCTE + `
SELECT
  model,
  request_count,
  CASE WHEN exact_speed THEN token_p50 ELSE char_p50 END,
  avg_first_token_ms,
  CASE WHEN exact_output THEN output_tokens ELSE output_chars END,
  avg_duration_ms,
  requests_with_first_token, exact_speed,exact_output
FROM stats
ORDER BY request_count DESC, model ASC`

	args := make([]any, 0, len(baseArgs)+2)
	args = append(args, baseArgs...)

	if filter.IsTopNMode() {
		querySQL += fmt.Sprintf("\nLIMIT $%d", next)
		args = append(args, filter.TopN)
	} else {
		offset := (filter.Page - 1) * filter.PageSize
		querySQL += fmt.Sprintf("\nLIMIT $%d OFFSET $%d", next, next+1)
		args = append(args, filter.PageSize, offset)
	}

	rows, err := r.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]*service.OpsOpenAITokenStatsItem, 0, 32)
	for rows.Next() {
		item := &service.OpsOpenAITokenStatsItem{}
		var avgTPS sql.NullFloat64
		var avgFirstToken sql.NullFloat64
		var exactSpeed, exactOutput bool
		if err := rows.Scan(
			&item.Model,
			&item.RequestCount,
			&avgTPS,
			&avgFirstToken,
			&item.Output,
			&item.AvgDurationMs,
			&item.RequestsWithFirstToken,
			&exactSpeed, &exactOutput,
		); err != nil {
			return nil, err
		}
		if avgTPS.Valid {
			v := avgTPS.Float64
			item.P50 = &v
		}
		if avgFirstToken.Valid {
			v := avgFirstToken.Float64
			item.AvgFirstTokenMs = &v
		}
		items = append(items, item)
		item.SpeedUnit = "char"
		item.Unit = "char"
		if exactSpeed {
			item.SpeedUnit = "token"
		}
		if exactOutput {
			item.Unit = "token"
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	resp := &service.OpsOpenAITokenStatsResponse{
		TimeRange: strings.TrimSpace(filter.TimeRange),
		StartTime: dashboardFilter.StartTime,
		EndTime:   dashboardFilter.EndTime,
		Platform:  dashboardFilter.Platform,
		GroupID:   dashboardFilter.GroupID,
		Items:     items,
		Total:     total,
	}
	if filter.IsTopNMode() {
		topN := filter.TopN
		resp.TopN = &topN
	} else {
		resp.Page = filter.Page
		resp.PageSize = filter.PageSize
	}
	return resp, nil
}
