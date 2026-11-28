package opsdelivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpsDeliveryObservesVisibleWritesAcrossMinute(t *testing.T) {
	start := time.Date(2026, 11, 28, 5, 55, 56, 0, time.UTC)
	collector := &Collector{rows: map[string]Row{}, buckets: map[bucketKey]Bucket{}}
	_, session := WithSession(context.Background(), collector, "request")
	session.Start(start, "model")
	session.Attempt(503)
	session.Attempt(200)
	body := Body{Session: session}
	write := func(after time.Duration, data string) {
		body.Write(start.Add(after), "text/event-stream", []byte(data))
	}
	write(time.Second, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"hidden\"}\n\n")
	write(time.Second, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"tool\"}\n\n")
	write(2*time.Second, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"01234")
	write(2*time.Second, "56789\"}\r\n\r\n")
	write(6*time.Second, "data: {\"type\":\"response.output_text.delta\",\"delta\":\""+strings.Repeat("测", 80)+"\"}\n\n")
	write(7*time.Second, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":100000,\"input_tokens_details\":{\"cached_tokens\":100000}},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"duplicate snapshot\"}]}]}}\n\n")
	session.Finish(start.Add(7*time.Second), body.Outcome(200, false, false, false))
	row := session.row
	require.EqualValues(t, 2000, *row.FirstMs)
	require.EqualValues(t, 20, float64(row.Chars-row.FirstChars)*1000 / *row.SpanMs)
	require.EqualValues(t, 90, row.Chars)
	require.EqualValues(t, 90, row.UnknownChars)
	require.EqualValues(t, 2, row.Attempts)
	require.EqualValues(t, 1, row.UpstreamErrors)
	require.Equal(t, "completed", row.Outcome)
	require.Len(t, collector.buckets, 2)
	require.EqualValues(t, 10, collector.buckets[bucketKey{row.ID, start.Truncate(Window).Unix()}].Chars)
	require.EqualValues(t, 80, collector.buckets[bucketKey{row.ID, start.Add(6 * time.Second).Truncate(Window).Unix()}].Chars)
}

func TestOpsDeliverySingleWriteAndTerminalState(t *testing.T) {
	start := time.Now()
	collector := &Collector{rows: map[string]Row{}, buckets: map[bucketKey]Bucket{}}
	_, session := WithSession(context.Background(), collector, "request")
	session.Start(start, "model")
	body := Body{Session: session}
	body.Write(start.Add(time.Second), "text/event-stream", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ab\"},\"logprobs\":{\"content\":[{\"bytes\":[97,98]}]}}]}\n\n"))
	require.EqualValues(t, 1, session.row.Tokens)
	require.Zero(t, *session.row.SpanMs)
	require.Equal(t, "failed", body.Outcome(200, false, false, false))
	require.Equal(t, "cancelled", body.Outcome(200, true, false, false))
	body.Write(start.Add(2*time.Second), "text/event-stream", []byte("data: [DONE]\n\n"))
	require.Equal(t, "completed", body.Outcome(200, false, false, false))
}
