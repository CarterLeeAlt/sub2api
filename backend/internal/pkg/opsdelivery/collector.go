// Package opsdelivery measures downstream writes independently of billing usage.
package opsdelivery

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const Window = 5 * time.Second

// Bound the amount of uncommitted monitoring data independently of request traffic.
const maxPendingMeasurements = 65536

type Counts struct{ Chars, Tokens, UnknownChars int64 }
type Row struct {
	ID                                  string
	Started                             time.Time
	Ended                               *time.Time
	RequestID, Platform, Model, Outcome string
	GroupID                             int64
	FirstMs, SpanMs                     *float64
	Counts
	FirstChars, FirstTokens                   int64
	Attempts, UpstreamErrors, LimitedAttempts int
	Incomplete                                bool
	UserID, APIKeyID, AccountID               *int64
	Stream                                    bool
}
type Bucket struct {
	ID     string
	Bucket time.Time
	Counts
}
type Store interface {
	SaveDelivery(context.Context, []Row, []Bucket, []time.Time) error
}
type Collector struct {
	mu         sync.Mutex
	rows       map[string]Row
	buckets    map[bucketKey]Bucket
	stop, done chan struct{}
	once       sync.Once
	lostUntil  time.Time
}
type bucketKey struct {
	id string
	ts int64
}

func NewCollector(store Store, enabled func() bool) *Collector {
	c := &Collector{rows: make(map[string]Row), buckets: make(map[bucketKey]Bucket), stop: make(chan struct{}), done: make(chan struct{})}
	go c.run(store, enabled)
	return c
}
func (c *Collector) Stop() {
	if c != nil {
		c.once.Do(func() { close(c.stop) })
		<-c.done
	}
}
func (c *Collector) run(store Store, enabled func() bool) {
	defer close(c.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	next := time.Now().UTC().Truncate(Window).Add(Window)
	for {
		stopping := false
		select {
		case <-ticker.C:
		case <-c.stop:
			stopping = true
		}
		end := time.Now().UTC().Truncate(Window)
		var covered []time.Time
		c.mu.Lock()
		rows, buckets := c.rows, c.buckets
		if c.lostUntil.After(next) {
			next = c.lostUntil
		}
		c.rows = make(map[string]Row)
		c.buckets = make(map[bucketKey]Bucket)
		c.mu.Unlock()
		if enabled() {
			for t := next; t.Before(end); t = t.Add(Window) {
				covered = append(covered, t)
			}
		}
		if len(rows)+len(buckets)+len(covered) > 0 {
			batch := make([]Row, 0, len(rows))
			for _, r := range rows {
				batch = append(batch, r)
			}
			counts := make([]Bucket, 0, len(buckets))
			for _, b := range buckets {
				counts = append(counts, b)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := store.SaveDelivery(ctx, batch, counts, covered)
			cancel()
			if err != nil {
				slog.Error("persist ops delivery measurements", "error", err)
				c.mu.Lock()
				if len(rows)+len(buckets)+len(c.rows)+len(c.buckets) > maxPendingMeasurements || end.Sub(next) > time.Minute {
					c.markLost(time.Now())
				} else {
					for k, r := range rows {
						if _, newer := c.rows[k]; !newer {
							c.rows[k] = r
						}
					}
					for k, b := range buckets {
						if _, newer := c.buckets[k]; !newer {
							c.buckets[k] = b
						}
					}
				}
				c.mu.Unlock()
				if stopping {
					return
				}
				continue
			}
		}
		if end.After(next) {
			next = end
		}
		if stopping {
			return
		}
	}
}

func (c *Collector) markLost(at time.Time) {
	c.lostUntil = at.UTC().Truncate(Window).Add(Window)
}

type contextKey struct{}
type Session struct {
	mu                         sync.Mutex
	collector                  *Collector
	requestID, platform, model string
	groupID                    int64
	row                        Row
	first                      time.Time
	bucket                     Bucket
	lastAttemptOpen            bool
}

func WithSession(ctx context.Context, c *Collector, id string) (context.Context, *Session) {
	s := &Session{collector: c, requestID: id}
	return context.WithValue(ctx, contextKey{}, s), s
}
func From(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(contextKey{}).(*Session)
	return s
}
func (s *Session) Start(at time.Time, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID != "" && s.row.Ended == nil {
		return
	}
	s.first = time.Time{}
	s.bucket = Bucket{}
	s.lastAttemptOpen = false
	s.row = Row{ID: uuid.NewString(), Started: at.UTC(), RequestID: s.requestID, Platform: s.platform, GroupID: s.groupID, Model: model, Outcome: "active"}
	s.save()
}
func (s *Session) Dimensions(platform string, group int64, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.platform, s.groupID = platform, group
	s.row.Platform, s.row.GroupID = platform, group
	if model != "" {
		s.row.Model = model
	}
	if s.row.ID != "" {
		s.save()
	}
}
func (s *Session) save() {
	s.collector.mu.Lock()
	defer s.collector.mu.Unlock()
	if len(s.collector.rows)+len(s.collector.buckets) >= maxPendingMeasurements {
		s.collector.markLost(time.Now())
		s.row.Incomplete = true
		return
	}
	s.collector.rows[s.row.ID] = s.row
}
func (s *Session) Identity(user, key, account int64, stream bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" {
		return
	}
	if user > 0 {
		s.row.UserID = &user
	}
	if key > 0 {
		s.row.APIKeyID = &key
	}
	if account > 0 {
		s.row.AccountID = &account
	}
	s.row.Stream = stream
	s.save()
}
func (s *Session) Output(at time.Time, n Counts) {
	if s == nil || n.Chars <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" || s.row.Ended != nil {
		return
	}
	if s.first.IsZero() {
		s.first = at
		s.row.FirstChars = n.Chars
		s.row.FirstTokens = n.Tokens
		ms := float64(at.Sub(s.row.Started)) / float64(time.Millisecond)
		s.row.FirstMs = &ms
	} else if at.Equal(s.first) {
		s.row.FirstChars += n.Chars
		s.row.FirstTokens += n.Tokens
	}
	span := float64(at.Sub(s.first)) / float64(time.Millisecond)
	s.row.SpanMs = &span
	t := at.UTC().Truncate(Window)
	if !s.bucket.Bucket.Equal(t) {
		s.bucket = Bucket{ID: s.row.ID, Bucket: t}
	}
	s.bucket.Chars += n.Chars
	s.bucket.Tokens += n.Tokens
	s.bucket.UnknownChars += n.UnknownChars
	s.row.Chars += n.Chars
	s.row.Tokens += n.Tokens
	s.row.UnknownChars += n.UnknownChars
	s.collector.mu.Lock()
	defer s.collector.mu.Unlock()
	if len(s.collector.rows)+len(s.collector.buckets) >= maxPendingMeasurements {
		s.collector.markLost(at)
		s.row.Incomplete = true
		return
	}
	s.collector.rows[s.row.ID] = s.row
	s.collector.buckets[bucketKey{s.row.ID, t.Unix()}] = s.bucket
}
func (s *Session) Attempt(status int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" || s.row.Ended != nil {
		return
	}
	s.row.Attempts++
	s.lastAttemptOpen = status > 0 && status < 400
	switch {
	case status == 429 || status == 529:
		s.row.LimitedAttempts++
	case status == 0 || status >= 400:
		s.row.UpstreamErrors++
	}
	s.save()
}
func (s *Session) Finish(at time.Time, outcome string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" || s.row.Ended != nil {
		return
	}
	s.row.Outcome = outcome
	s.row.Ended = &at
	s.save()
}

// FailAttempt classifies a previously opened attempt whose stream terminated with an error.
func (s *Session) FailAttempt(status int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" || s.row.Ended != nil || !s.lastAttemptOpen {
		return
	}
	s.lastAttemptOpen = false
	if status == 429 || status == 529 {
		s.row.LimitedAttempts++
	} else {
		s.row.UpstreamErrors++
	}
	s.save()
}
func (s *Session) Missing() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID != "" {
		s.row.Incomplete = true
		s.save()
	}
}
