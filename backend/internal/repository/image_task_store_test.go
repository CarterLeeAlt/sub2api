package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestImageTaskStoreRoundTripAndTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := NewImageTaskStore(rdb)
	task := &service.ImageTaskRecord{
		ID:        "imgtask_123",
		UserID:    7,
		APIKeyID:  9,
		Status:    service.ImageTaskStatusProcessing,
		CreatedAt: 100,
		ExpiresAt: 200,
	}

	require.NoError(t, store.Save(context.Background(), task, 24*time.Hour))
	got, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.Equal(t, task, got)
	require.Equal(t, 24*time.Hour, mr.TTL(imageTaskKey(task.ID)))
}

func TestImageTaskStoreMissing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := NewImageTaskStore(rdb)

	_, err := store.Get(context.Background(), "imgtask_missing")
	require.ErrorIs(t, err, service.ErrImageTaskNotFound)
}

func TestImageTaskTerminalResultIsAtomicAndDeadlineBound(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := NewImageTaskStore(rdb)
	start := time.Now().UTC().Truncate(time.Millisecond)
	deadline := start.Add(30 * time.Minute)
	for _, tc := range []struct {
		name    string
		at      time.Time
		expired bool
	}{
		{"before_deadline", deadline.Add(-time.Millisecond), false},
		{"at_deadline", deadline, true},
		{"after_deadline", deadline.Add(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr.SetTime(tc.at)
			base := service.ImageTaskRecord{ID: tc.name, Status: service.ImageTaskStatusProcessing, DeadlineMS: deadline.UnixMilli()}
			require.NoError(t, store.Save(t.Context(), &base, time.Hour))
			success, failure, timeout := base, base, base
			success.Status, success.Result = service.ImageTaskStatusCompleted, json.RawMessage(`{"data":[{"url":"image"}]}`)
			failure.Status, failure.HTTPStatus = service.ImageTaskStatusFailed, 502
			timeout.Status, timeout.HTTPStatus = service.ImageTaskStatusFailed, 504
			var wg sync.WaitGroup
			startFinish := make(chan struct{})
			observed := make(chan *service.ImageTaskRecord, 2)
			for _, candidate := range []*service.ImageTaskRecord{&success, &failure} {
				wg.Go(func() {
					<-startFinish
					result, err := store.FinishProcessing(t.Context(), candidate, &timeout, deadline, time.Hour)
					if err != nil {
						t.Error(err)
						return
					}
					observed <- result
				})
			}
			close(startFinish)
			wg.Wait()
			close(observed)
			winner, err := store.Get(t.Context(), base.ID)
			require.NoError(t, err)
			for result := range observed {
				require.Equal(t, winner, result)
			}
			if tc.expired {
				require.Equal(t, timeout, *winner)
			} else {
				require.Contains(t, []service.ImageTaskRecord{success, failure}, *winner)
			}
		})
	}
}
