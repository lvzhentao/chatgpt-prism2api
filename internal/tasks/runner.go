package tasks

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Runner 按全局并发执行长任务。
type Runner struct {
	store   *Store
	max     atomic.Int64
	mu      sync.Mutex
	active  int
	waiters []chan struct{}
	cancels map[string]context.CancelFunc
}

// NewRunner 创建执行器。concurrency 默认 4。
func NewRunner(store *Store, concurrency int) *Runner {
	r := &Runner{store: store, cancels: map[string]context.CancelFunc{}}
	r.SetConcurrency(concurrency)
	return r
}

// SetConcurrency 热更新并发上限（对后续取槽生效）。
func (r *Runner) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > 64 {
		n = 64
	}
	r.max.Store(int64(n))
}

// Concurrency 当前上限。
func (r *Runner) Concurrency() int {
	n := int(r.max.Load())
	if n < 1 {
		return 4
	}
	return n
}

func (r *Runner) acquire() {
	r.mu.Lock()
	if r.active < r.Concurrency() {
		r.active++
		r.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	r.waiters = append(r.waiters, ch)
	r.mu.Unlock()
	<-ch
}

func (r *Runner) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.waiters) > 0 {
		ch := r.waiters[0]
		r.waiters = r.waiters[1:]
		close(ch)
		return
	}
	if r.active > 0 {
		r.active--
	}
}

// Enqueue 登记并异步执行。
func (r *Runner) Enqueue(req CreateRequest) *Task {
	t := &Task{
		ID:         newID(),
		Type:       req.Type,
		Title:      req.Title,
		Status:     StatusPending,
		CreatedAt:  nowTime(),
		Total:      req.Total,
		Meta:       req.Meta,
		CreatedBy:  req.CreatedBy,
		Cancelable: req.Cancelable,
		Logs:       []LogLine{},
	}
	if t.Title == "" {
		t.Title = t.Type
	}
	r.store.mu.Lock()
	r.store.putLocked(t)
	r.store.mu.Unlock()
	go r.run(t.ID, req.Run)
	return cloneTask(t)
}

func (r *Runner) run(id string, fn func(*Context) (any, error)) {
	r.acquire()
	defer r.release()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		cancel()
	}()

	r.store.setStatus(id, StatusRunning, "", nil)
	handle := &Context{ctx: ctx, cancel: cancel, store: r.store, id: id}
	if fn == nil {
		r.store.setStatus(id, StatusFailed, "no runner", nil)
		return
	}
	result, err := fn(handle)
	if handle.Err() != nil {
		r.store.setStatus(id, StatusCanceled, handle.Err().Error(), result)
		return
	}
	if err != nil {
		handle.Error(err.Error())
		r.store.setStatus(id, StatusFailed, err.Error(), result)
		return
	}
	r.store.setStatus(id, StatusSuccess, "", result)
}

// Cancel 取消进行中的任务。
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	if t, exists := r.store.Get(id); !exists {
		return fmt.Errorf("task not found")
	} else if t.Status != StatusPending && t.Status != StatusRunning && t.Status != StatusCanceled {
		if !ok {
			return fmt.Errorf("task is %s", t.Status)
		}
	}
	r.store.markCanceled(id)
	return nil
}

// Store 暴露存储。
func (r *Runner) Store() *Store { return r.store }
