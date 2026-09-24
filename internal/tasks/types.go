package tasks

import "time"

const (
	StatusPending  = "pending"
	StatusRunning  = "running"
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusPartial  = "partial"
)

const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Task 可回溯的后台任务。
type Task struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"created_at"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Current    int            `json:"current"`
	Total      int            `json:"total"`
	Message    string         `json:"message,omitempty"`
	Error      string         `json:"error,omitempty"`
	Result     any            `json:"result,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Logs       []LogLine      `json:"logs"`
	CreatedBy  string         `json:"created_by,omitempty"`
	Cancelable bool           `json:"cancelable"`
}

// LogLine 一条任务日志（SSE 同步推送）。
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Event SSE 负载。
type Event struct {
	Type string   `json:"type"` // log | progress | status
	Task *Task    `json:"task,omitempty"`
	Log  *LogLine `json:"log,omitempty"`
}

// CreateRequest 入队。
type CreateRequest struct {
	Type       string
	Title      string
	Total      int
	Meta       map[string]any
	CreatedBy  string
	Cancelable bool
	Run        func(ctx *Context) (any, error)
}
