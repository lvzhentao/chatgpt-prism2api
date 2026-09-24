// Package tasks 是管理台任务中心：可回溯记录 + 每任务 SSE 日志。
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/persist"
)

const kindDoc = persist.KindTasks

const maxTasks = 400
const maxLogs = 2000

// Store 任务存储 + 订阅。
type Store struct {
	mu      sync.Mutex
	items   map[string]*Task
	order   []string
	backend persist.Backend
	subs    map[string]map[int]chan Event
	subID   int
}

// Open 加载历史任务。
func Open(b persist.Backend) (*Store, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	s := &Store{items: map[string]*Task{}, backend: b, subs: map[string]map[int]chan Event{}}
	data, err := b.LoadDoc(kindDoc, persist.IDAll)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return s, nil
	}
	var list []Task
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	for i := range list {
		t := list[i]
		if t.Status == StatusRunning || t.Status == StatusPending {
			t.Status = StatusFailed
			t.Error = "interrupted"
			now := time.Now()
			t.FinishedAt = &now
		}
		cp := t
		s.items[t.ID] = &cp
		s.order = append(s.order, t.ID)
	}
	s.trimLocked()
	return s, nil
}

func (s *Store) saveLocked() {
	if s.backend == nil {
		return
	}
	list := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		if t := s.items[id]; t != nil {
			list = append(list, *t)
		}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	_ = s.backend.SaveDoc(kindDoc, persist.IDAll, b)
}

func (s *Store) trimLocked() {
	for len(s.order) > maxTasks {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.items, old)
	}
}

// List 最新在前。
func (s *Store) List(limit int) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := make([]Task, 0, limit)
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		t := s.items[s.order[i]]
		if t == nil {
			continue
		}
		cp := *t
		if len(cp.Logs) > 40 {
			cp.Logs = cp.Logs[len(cp.Logs)-40:]
		}
		out = append(out, cp)
	}
	return out
}

// Get 任务详情（含完整日志）。
func (s *Store) Get(id string) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.items[id]
	if !ok {
		return nil, false
	}
	cp := *t
	cp.Logs = append([]LogLine(nil), t.Logs...)
	return &cp, true
}

func (s *Store) putLocked(t *Task) {
	if _, ok := s.items[t.ID]; !ok {
		s.order = append(s.order, t.ID)
	}
	s.items[t.ID] = t
	s.trimLocked()
	s.saveLocked()
}

// Subscribe 订阅单任务事件。id 为空则订阅全部。
func (s *Store) Subscribe(id string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subID++
	sid := s.subID
	if s.subs[id] == nil {
		s.subs[id] = map[int]chan Event{}
	}
	s.subs[id][sid] = ch
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs[id], sid)
			s.mu.Unlock()
		})
	}
}

func (s *Store) publishLocked(id string, ev Event) {
	push := func(m map[int]chan Event) {
		for _, ch := range m {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	push(s.subs[id])
	push(s.subs[""])
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "task_" + hex.EncodeToString(b[:])
}

// Context 运行中任务句柄。
type Context struct {
	ctx    context.Context
	cancel context.CancelFunc
	store  *Store
	id     string
}

// Context 返回底层 context（给需要 context.Context 的外部调用用，如侧车登录）。
func (c *Context) Context() context.Context { return c.ctx }

func (c *Context) Done() <-chan struct{} { return c.ctx.Done() }
func (c *Context) Err() error            { return c.ctx.Err() }
func (c *Context) ID() string            { return c.id }

func (c *Context) Log(level, msg string) {
	c.store.appendLog(c.id, level, msg)
}

func (c *Context) Info(msg string)  { c.Log(LevelInfo, msg) }
func (c *Context) Warn(msg string)  { c.Log(LevelWarn, msg) }
func (c *Context) Error(msg string) { c.Log(LevelError, msg) }

func (c *Context) Progress(current, total int, message string) {
	c.store.progress(c.id, current, total, message)
}

func (s *Store) appendLog(id, level, msg string) {
	line := LogLine{Time: time.Now(), Level: level, Message: msg}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.items[id]
	if t == nil {
		return
	}
	t.Logs = append(t.Logs, line)
	if len(t.Logs) > maxLogs {
		t.Logs = t.Logs[len(t.Logs)-maxLogs:]
	}
	t.Message = msg
	s.saveLocked()
	s.publishLocked(id, Event{Type: "log", Task: cloneTask(t), Log: &line})
}

func (s *Store) progress(id string, current, total int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.items[id]
	if t == nil {
		return
	}
	t.Current = current
	if total > 0 {
		t.Total = total
	}
	if message != "" {
		t.Message = message
	}
	s.saveLocked()
	s.publishLocked(id, Event{Type: "progress", Task: cloneTask(t)})
}

func (s *Store) setStatus(id, status, errMsg string, result any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.items[id]
	if t == nil {
		return
	}
	t.Status = status
	if errMsg != "" {
		t.Error = errMsg
	}
	if result != nil {
		t.Result = result
	}
	now := time.Now()
	if status == StatusRunning && t.StartedAt == nil {
		t.StartedAt = &now
	}
	if status == StatusSuccess || status == StatusFailed || status == StatusCanceled || status == StatusPartial {
		t.FinishedAt = &now
	}
	s.saveLocked()
	s.publishLocked(id, Event{Type: "status", Task: cloneTask(t)})
}

func (s *Store) markCanceled(id string) bool {
	s.mu.Lock()
	t := s.items[id]
	if t == nil || (t.Status != StatusPending && t.Status != StatusRunning) {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	s.setStatus(id, StatusCanceled, "canceled", nil)
	return true
}

// Snapshot 给 SSE 首包。
func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Logs = append([]LogLine(nil), t.Logs...)
	return &cp
}

func nowTime() time.Time { return time.Now() }
