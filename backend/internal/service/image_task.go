package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	ImageTaskStatusProcessing = "processing"
	ImageTaskStatusCompleted  = "completed"
	ImageTaskStatusFailed     = "failed"

	defaultImageTaskTTL              = 24 * time.Hour
	defaultImageTaskExecutionTimeout = 30 * time.Minute
)

var (
	ErrImageTaskNotFound    = infraerrors.New(http.StatusNotFound, "IMAGE_TASK_NOT_FOUND", "image task not found")
	ErrImageTaskForbidden   = infraerrors.New(http.StatusForbidden, "IMAGE_TASK_FORBIDDEN", "image task does not belong to this API key")
	ErrImageTaskUnavailable = infraerrors.New(http.StatusServiceUnavailable, "IMAGE_TASK_UNAVAILABLE", "image task storage is unavailable")
)

// ImageTaskRecord is the private Redis representation of an asynchronous image
// request. Ownership fields are intentionally omitted from the public view.
type ImageTaskRecord struct {
	ID          string          `json:"id"`
	UserID      int64           `json:"user_id"`
	APIKeyID    int64           `json:"api_key_id"`
	Status      string          `json:"status"`
	HTTPStatus  int             `json:"http_status,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	DeadlineMS  int64           `json:"deadline_ms,omitempty"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	ExpiresAt   int64           `json:"expires_at"`
}

// ImageTask is the API-safe task representation returned to callers.
type ImageTask struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	Object      string          `json:"object"`
	Status      string          `json:"status"`
	HTTPStatus  int             `json:"http_status,omitempty"`
	ImageURL    string          `json:"image_url,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	ExpiresAt   int64           `json:"expires_at"`
}

type ImageTaskOwner struct {
	UserID   int64
	APIKeyID int64
}

type ImageTaskStore interface {
	Save(ctx context.Context, task *ImageTaskRecord, ttl time.Duration) error
	Get(ctx context.Context, id string) (*ImageTaskRecord, error)
	// FinishProcessing returns the stored winner. At or past deadline, timeout wins.
	FinishProcessing(ctx context.Context, candidate, timeout *ImageTaskRecord, deadline time.Time, ttl time.Duration) (*ImageTaskRecord, error)
}

// ImageStorageResolver reports the currently effective object-storage binding.
// It exists so the async image feature can be switched on and off from the admin
// UI without a restart: the wiring below is fixed at startup, but the answer to
// "is object storage configured right now" is re-read (and cached) per call.
type ImageStorageResolver func() (uploader *ImageResultUploader, enabled bool)

type ImageTaskService struct {
	store            ImageTaskStore
	uploader         *ImageResultUploader
	enabled          bool
	resolve          ImageStorageResolver
	ttl              time.Duration
	executionTimeout time.Duration
}

func NewImageTaskService(store ImageTaskStore) *ImageTaskService {
	return NewImageTaskServiceWithOptions(store, defaultImageTaskTTL, defaultImageTaskExecutionTimeout)
}

func NewImageTaskServiceWithOptions(store ImageTaskStore, ttl, executionTimeout time.Duration) *ImageTaskService {
	if ttl <= 0 {
		ttl = defaultImageTaskTTL
	}
	if executionTimeout <= 0 {
		executionTimeout = defaultImageTaskExecutionTimeout
	}
	return &ImageTaskService{store: store, ttl: ttl, executionTimeout: executionTimeout}
}

// NewImageTaskServiceWithUploader 构造一个已启用的图片任务服务：结果会先经 uploader
// 转存到对象存储再落 Redis。uploader 为 nil 时不做转存（仅用于测试）。
func NewImageTaskServiceWithUploader(store ImageTaskStore, uploader *ImageResultUploader, ttl, executionTimeout time.Duration) *ImageTaskService {
	s := NewImageTaskServiceWithOptions(store, ttl, executionTimeout)
	s.uploader = uploader
	s.enabled = true
	return s
}

// NewImageTaskServiceWithResolver 构造一个由 resolver 决定启用状态的服务：
// 开关与凭证来自后台设置，保存后立即生效，无需重启。
func NewImageTaskServiceWithResolver(store ImageTaskStore, resolve ImageStorageResolver, ttl, executionTimeout time.Duration) *ImageTaskService {
	s := NewImageTaskServiceWithOptions(store, ttl, executionTimeout)
	s.resolve = resolve
	return s
}

// current 返回当前生效的 uploader 与启用状态。
// 注入了 resolver 时以 resolver 为准（后台设置可热切换），否则回落到构造时固定的值。
func (s *ImageTaskService) current() (*ImageResultUploader, bool) {
	if s == nil {
		return nil, false
	}
	if s.resolve != nil {
		return s.resolve()
	}
	return s.uploader, s.enabled
}

// Enabled 表示异步图片任务功能是否可用（总开关 + 凭证齐全）。
// 关闭时 handler 直接返回 404，不创建任务、不写 Redis。
func (s *ImageTaskService) Enabled() bool {
	_, enabled := s.SubmissionStorage()
	return enabled
}

// SubmissionStorage pins the storage binding for the lifetime of an accepted task.
func (s *ImageTaskService) SubmissionStorage() (*ImageResultUploader, bool) {
	if s == nil || s.store == nil {
		return nil, false
	}
	return s.current()
}

// Pollable 表示已创建的任务能否被查询。
// 比 Enabled 弱：只要 store 可用即可，从而在功能被关掉后仍能取回进行中的任务结果。
func (s *ImageTaskService) Pollable() bool {
	return s != nil && s.store != nil
}

func (s *ImageTaskService) ExecutionTimeout() time.Duration {
	if s == nil || s.executionTimeout <= 0 {
		return defaultImageTaskExecutionTimeout
	}
	return s.executionTimeout
}

func (s *ImageTaskService) Create(ctx context.Context, owner ImageTaskOwner) (*ImageTask, error) {
	if s == nil || s.store == nil {
		return nil, ErrImageTaskUnavailable
	}
	now := time.Now().UTC()
	task := &ImageTaskRecord{
		ID:         "imgtask_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		UserID:     owner.UserID,
		APIKeyID:   owner.APIKeyID,
		Status:     ImageTaskStatusProcessing,
		CreatedAt:  now.Unix(),
		DeadlineMS: now.Add(s.executionTimeout).UnixMilli(),
		ExpiresAt:  now.Add(s.ttl).Unix(),
	}
	if err := s.store.Save(ctx, task, s.ttl); err != nil {
		return nil, ErrImageTaskUnavailable.WithCause(err)
	}
	return imageTaskToPublic(task), nil
}

func (s *ImageTaskService) Get(ctx context.Context, owner ImageTaskOwner, id string) (*ImageTask, error) {
	if s == nil || s.store == nil {
		return nil, ErrImageTaskUnavailable
	}
	task, err := s.store.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, ErrImageTaskNotFound) {
			return nil, ErrImageTaskNotFound
		}
		return nil, ErrImageTaskUnavailable.WithCause(err)
	}
	if task.UserID != owner.UserID || task.APIKeyID != owner.APIKeyID {
		// Do not reveal whether a random task ID exists for another caller.
		return nil, ErrImageTaskNotFound
	}
	// 惰性 reaper：任务执行是进程内 goroutine，进程崩溃/重启后 processing 记录
	// 会永久滞留（只能等 TTL 过期）。轮询时超过执行时限仍未终态的任务按超时
	// 标 failed，使文档"任务最多执行 executionTimeout"的承诺在崩溃场景下同样
	// 成立，客户端也不会无限轮询。
	if task.Status == ImageTaskStatusProcessing && !time.Now().Before(s.deadline(task)) {
		task, err = s.finish(ctx, task, ImageTaskStatusFailed, http.StatusGatewayTimeout, nil, imageTaskErrorJSON("timeout_error", "image task timed out"))
		if err != nil {
			return nil, err
		}
	}
	return imageTaskToPublic(task), nil
}

func (s *ImageTaskService) deadline(task *ImageTaskRecord) time.Time {
	if task.DeadlineMS != 0 {
		return time.UnixMilli(task.DeadlineMS)
	}
	return time.Unix(task.CreatedAt, 0).Add(s.executionTimeout)
}

// Deliver returns true only when the stored terminal result is available to the caller.
func (s *ImageTaskService) Deliver(ctx context.Context, id string, statusCode int, result json.RawMessage, uploader *ImageResultUploader) (bool, error) {
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if task.Status != ImageTaskStatusProcessing {
		return task.Status == ImageTaskStatusCompleted, nil
	}
	var taskErr json.RawMessage
	if !time.Now().Before(s.deadline(task)) {
		taskErr = imageTaskErrorJSON("timeout_error", "image task timed out")
	} else if !json.Valid(result) {
		taskErr = imageTaskErrorJSON("api_error", "upstream returned a non-JSON image response")
	} else if countOpenAIResponseImageOutputsFromJSONBytes(result) == 0 {
		taskErr = imageTaskErrorJSON("api_error", "upstream returned no image output")
	} else if uploader != nil {
		uploadCtx, cancel := context.WithDeadline(ctx, s.deadline(task))
		rewritten, err := uploader.Rewrite(uploadCtx, id, result)
		cancel()
		if err != nil {
			logger.L().Error("image_task.offload_failed", zap.String("task_id", id), zap.Error(err))
			taskErr = imageTaskErrorJSON("api_error", "failed to store generated image to object storage")
		}
		result = rewritten
	}
	status := ImageTaskStatusCompleted
	if taskErr != nil {
		status, statusCode, result = ImageTaskStatusFailed, http.StatusBadGateway, nil
	}
	winner, err := s.finish(ctx, task, status, statusCode, result, taskErr)
	return err == nil && winner.Status == ImageTaskStatusCompleted, err
}

func (s *ImageTaskService) Fail(ctx context.Context, id string, statusCode int, taskErr json.RawMessage) error {
	if !json.Valid(taskErr) {
		taskErr = imageTaskErrorJSON("api_error", "image generation failed")
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.finish(ctx, task, ImageTaskStatusFailed, statusCode, nil, taskErr)
	return err
}

func (s *ImageTaskService) finish(ctx context.Context, task *ImageTaskRecord, status string, statusCode int, result, taskErr json.RawMessage) (*ImageTaskRecord, error) {
	now := time.Now().UTC()
	completedAt := now.Unix()
	task.Status = status
	task.HTTPStatus = statusCode
	task.Result = result
	task.Error = taskErr
	task.CompletedAt = &completedAt
	task.ExpiresAt = now.Add(s.ttl).Unix()
	timeout := *task
	timeout.Status, timeout.HTTPStatus, timeout.Result = ImageTaskStatusFailed, http.StatusGatewayTimeout, nil
	timeout.Error = imageTaskErrorJSON("timeout_error", "image task timed out")
	return s.store.FinishProcessing(ctx, task, &timeout, s.deadline(task), s.ttl)
}

func imageTaskToPublic(task *ImageTaskRecord) *ImageTask {
	if task == nil {
		return nil
	}
	return &ImageTask{
		ID:          task.ID,
		TaskID:      task.ID,
		Object:      "image.generation.task",
		Status:      task.Status,
		HTTPStatus:  task.HTTPStatus,
		ImageURL:    firstImageTaskURL(task.Result),
		Result:      task.Result,
		Error:       task.Error,
		CreatedAt:   task.CreatedAt,
		CompletedAt: task.CompletedAt,
		ExpiresAt:   task.ExpiresAt,
	}
}

func firstImageTaskURL(result json.RawMessage) string {
	if len(result) == 0 || !json.Valid(result) {
		return ""
	}
	var response struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if json.Unmarshal(result, &response) != nil || len(response.Data) == 0 {
		return ""
	}
	return strings.TrimSpace(response.Data[0].URL)
}

func imageTaskErrorJSON(errorType, message string) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"type": errorType, "message": message})
	return data
}
