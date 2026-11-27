package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const imageTaskKeyPrefix = "image_task:"

type imageTaskStore struct {
	rdb *redis.Client
}

func NewImageTaskStore(rdb *redis.Client) service.ImageTaskStore {
	return &imageTaskStore{rdb: rdb}
}

func (s *imageTaskStore) Save(ctx context.Context, task *service.ImageTaskRecord, ttl time.Duration) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, imageTaskKey(task.ID), data, ttl).Err()
}

func (s *imageTaskStore) Get(ctx context.Context, id string) (*service.ImageTaskRecord, error) {
	data, err := s.rdb.Get(ctx, imageTaskKey(id)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, service.ErrImageTaskNotFound
		}
		return nil, err
	}
	var task service.ImageTaskRecord
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func imageTaskKey(id string) string {
	return imageTaskKeyPrefix + strings.TrimSpace(id)
}

var finishImageTask = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return false end
if cjson.decode(current).status ~= 'processing' then return current end
local now = redis.call('TIME')
local winner = ARGV[1]
if now[1] * 1000 + math.floor(now[2] / 1000) >= tonumber(ARGV[3]) then winner = ARGV[2] end
redis.call('SET', KEYS[1], winner, 'PX', ARGV[4])
return winner
`)

func (s *imageTaskStore) FinishProcessing(ctx context.Context, candidate, timeout *service.ImageTaskRecord, deadline time.Time, ttl time.Duration) (*service.ImageTaskRecord, error) {
	data, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	expired, err := json.Marshal(timeout)
	if err != nil {
		return nil, err
	}
	raw, err := finishImageTask.Run(ctx, s.rdb, []string{imageTaskKey(candidate.ID)}, data, expired, deadline.UnixMilli(), ttl.Milliseconds()).Text()
	if err == redis.Nil {
		return nil, service.ErrImageTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	var winner service.ImageTaskRecord
	if err := json.Unmarshal([]byte(raw), &winner); err != nil {
		return nil, err
	}
	return &winner, nil
}
