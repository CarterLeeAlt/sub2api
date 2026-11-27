package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type imageTaskMemoryStore struct {
	task    *ImageTaskRecord
	ttl     time.Duration
	saveErr error
	getErr  error
}

func (s *imageTaskMemoryStore) Save(_ context.Context, task *ImageTaskRecord, ttl time.Duration) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	copy := *task
	s.task = &copy
	s.ttl = ttl
	return nil
}

func (s *imageTaskMemoryStore) Get(_ context.Context, _ string) (*ImageTaskRecord, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.task == nil {
		return nil, ErrImageTaskNotFound
	}
	copy := *s.task
	return &copy, nil
}

func (s *imageTaskMemoryStore) FinishProcessing(ctx context.Context, candidate, timeout *ImageTaskRecord, deadline time.Time, ttl time.Duration) (*ImageTaskRecord, error) {
	if s.task.Status != ImageTaskStatusProcessing {
		return s.Get(ctx, candidate.ID)
	}
	if !time.Now().Before(deadline) {
		candidate = timeout
	}
	return candidate, s.Save(ctx, candidate, ttl)
}

func TestImageTaskServiceLifecycleAndOwnership(t *testing.T) {
	store := &imageTaskMemoryStore{}
	svc := NewImageTaskServiceWithOptions(store, time.Hour, 10*time.Minute)
	owner := ImageTaskOwner{UserID: 7, APIKeyID: 9}

	created, err := svc.Create(context.Background(), owner)
	require.NoError(t, err)
	require.Equal(t, ImageTaskStatusProcessing, created.Status)
	require.Equal(t, created.ID, created.TaskID)
	require.Equal(t, "image.generation.task", created.Object)
	require.Equal(t, time.Hour, store.ttl)
	require.Equal(t, owner.UserID, store.task.UserID)
	require.Equal(t, owner.APIKeyID, store.task.APIKeyID)

	_, err = svc.Get(context.Background(), ImageTaskOwner{UserID: 7, APIKeyID: 10}, created.ID)
	require.ErrorIs(t, err, ErrImageTaskNotFound)

	result := json.RawMessage(`{"created":123,"data":[{"url":"https://example.test/image.png"}]}`)
	delivered, err := svc.Deliver(t.Context(), created.ID, http.StatusOK, result, nil)
	require.NoError(t, err)
	require.True(t, delivered)

	completed, err := svc.Get(context.Background(), owner, created.ID)
	require.NoError(t, err)
	require.Equal(t, ImageTaskStatusCompleted, completed.Status)
	require.Equal(t, http.StatusOK, completed.HTTPStatus)
	require.Equal(t, "https://example.test/image.png", completed.ImageURL)
	require.JSONEq(t, string(result), string(completed.Result))
	require.NotNil(t, completed.CompletedAt)
}

func TestImageTaskServiceInvalidResultBecomesFailed(t *testing.T) {
	store := &imageTaskMemoryStore{}
	svc := NewImageTaskServiceWithOptions(store, time.Hour, time.Minute)
	created, err := svc.Create(context.Background(), ImageTaskOwner{UserID: 1, APIKeyID: 2})
	require.NoError(t, err)

	delivered, err := svc.Deliver(t.Context(), created.ID, http.StatusOK, json.RawMessage(`not-json`), nil)
	require.NoError(t, err)
	require.False(t, delivered)
	got, err := svc.Get(context.Background(), ImageTaskOwner{UserID: 1, APIKeyID: 2}, created.ID)
	require.NoError(t, err)
	require.Equal(t, ImageTaskStatusFailed, got.Status)
	require.Equal(t, http.StatusBadGateway, got.HTTPStatus)
	require.Contains(t, string(got.Error), "non-JSON")
}

func TestImageTaskServiceMapsStoreFailures(t *testing.T) {
	store := &imageTaskMemoryStore{saveErr: errors.New("redis down")}
	svc := NewImageTaskService(store)

	_, err := svc.Create(context.Background(), ImageTaskOwner{UserID: 1, APIKeyID: 2})
	require.ErrorIs(t, err, ErrImageTaskUnavailable)
}

func TestImageTaskLateDeliveryStaysFailed(t *testing.T) {
	for _, poll := range []bool{false, true} {
		store := &imageTaskMemoryStore{}
		svc := NewImageTaskServiceWithOptions(store, time.Hour, 30*time.Minute)
		owner := ImageTaskOwner{UserID: 1, APIKeyID: 2}
		task, err := svc.Create(t.Context(), owner)
		require.NoError(t, err)
		// Records created before deadline_ms derive their deadline from created_at.
		store.task.DeadlineMS = 0
		store.task.CreatedAt = time.Now().Add(-31 * time.Minute).Unix()
		if poll {
			polled, err := svc.Get(t.Context(), owner, task.ID)
			require.NoError(t, err)
			require.Equal(t, ImageTaskStatusFailed, polled.Status)
		}
		delivered, err := svc.Deliver(t.Context(), task.ID, 200, json.RawMessage(`{"data":[{"url":"late"}]}`), nil)
		require.NoError(t, err)
		require.False(t, delivered)
		got, err := svc.Get(t.Context(), owner, task.ID)
		require.NoError(t, err)
		require.Equal(t, http.StatusGatewayTimeout, got.HTTPStatus)
		require.Empty(t, got.Result)
	}
}
