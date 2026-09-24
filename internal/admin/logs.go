package admin

import (
	"prism-2api/internal/failclass"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	EventLog      = "log"
	EventCooldown = "cooldown"
	EventLogin    = "login"
)

// LogEntry 一条请求日志。
type LogEntry struct {
	ID          int64             `json:"id"`
	Time        time.Time         `json:"time"`
	IP          string            `json:"ip"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Status      int               `json:"status"`
	Account     string            `json:"account,omitempty"`
	Model       string            `json:"model,omitempty"`
	Prompt      int               `json:"prompt_tokens,omitempty"`
	Completion  int               `json:"completion_tokens,omitempty"`
	Total       int               `json:"total_tokens,omitempty"`
	Latency     int64             `json:"latency_ms"`
	Error       string            `json:"error,omitempty"`
	ClientKeyID *uint64           `json:"client_key_id,omitempty"`
	FailClass   string            `json:"fail_class,omitempty"`
	RetryCount  int               `json:"retry_count,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	RequestBody string            `json:"request_body,omitempty"`
}

// Event 可观测总线事件：新日志 / 冷却变更 / 登录成功。
type Event struct {
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	Log       *LogEntry `json:"log,omitempty"`
	Account   string    `json:"account,omitempty"`
	FailClass string    `json:"fail_class,omitempty"`
	User      string    `json:"user,omitempty"`
}

// LogSink 可选持久层（PostgreSQL）。内存环仍服务 SSE；查询优先走 Sink。
type LogSink interface {
	InsertLog(e LogEntry) error
	QueryLogs(limit int, f LogFilter) ([]LogEntry, error)
	GetLog(id int64) (*LogEntry, error)
	ComputeLogStats() (Stats, error)
	ClearLogs() error
}

// LogFilter 请求日志筛选。
type LogFilter struct {
	Model     string
	Account   string
	Status    string
	Path      string
	FailClass string
}

// LogStore 环形缓冲请求日志（线程安全）。
type LogStore struct {
	mu      sync.RWMutex
	entries []LogEntry // 环形：索引由 next 递增
	next    int
	max     int
	subs    map[int]chan Event
	subID   int
	sink    LogSink
}

// NewLogStore 创建日志存储。
func NewLogStore(max int) *LogStore {
	if max < 1 {
		max = 1
	}
	return &LogStore{entries: make([]LogEntry, 0, max), max: max}
}

// SetSink 挂上 PostgreSQL 等持久层。
func (s *LogStore) SetSink(sink LogSink) {
	s.mu.Lock()
	s.sink = sink
	s.mu.Unlock()
}

// SetMax 动态调整容量（收缩时丢弃最旧数据）。
func (s *LogStore) SetMax(max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max < 1 {
		max = 1
	}
	if max == s.max {
		return
	}
	s.max = max
	if len(s.entries) > max {
		// 截断最旧的
		drop := len(s.entries) - max
		if s.next >= drop {
			s.entries = s.entries[drop:]
			s.next -= drop
		} else {
			s.entries = append(s.entries[s.next:], s.entries[:drop-s.next]...)
			s.next = 0
		}
	}
}

// Add 追加一条日志，并通知订阅者。
func (s *LogStore) Add(e LogEntry) {
	s.mu.Lock()
	if len(s.entries) < s.max {
		e.ID = int64(len(s.entries))
		s.entries = append(s.entries, e)
	} else {
		e.ID = int64(s.next)
		s.entries[s.next] = e
		s.next = (s.next + 1) % s.max
	}
	cp := e
	sink := s.sink
	s.mu.Unlock()
	s.publish(Event{Type: EventLog, Time: cp.Time, Log: &cp})
	if sink != nil {
		go func() { _ = sink.InsertLog(cp) }()
	}
}

// Clear 清空全部日志。
func (s *LogStore) Clear() {
	s.mu.Lock()
	s.entries = s.entries[:0]
	s.next = 0
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		_ = sink.ClearLogs()
	}
}

// Subscribe 订阅后续事件。退订后不再投递；慢消费者丢事件，不阻塞 Add。
func (s *LogStore) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	s.mu.Lock()
	s.subID++
	id := s.subID
	if s.subs == nil {
		s.subs = map[int]chan Event{}
	}
	s.subs[id] = ch
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
		})
	}
}

// Publish 投递非日志事件（冷却 / 登录）。
func (s *LogStore) Publish(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Type == "" {
		ev.Type = EventLog
	}
	s.publish(ev)
}

func (s *LogStore) publish(ev Event) {
	s.mu.Lock()
	if len(s.subs) == 0 {
		s.mu.Unlock()
		return
	}
	subs := make([]chan Event, 0, len(s.subs))
	for _, ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Query 按条件查询（最新在前，limit<=0 或超过容量时返回全部）。
// 支持按 model / account / status / path 子串 / fail_class 筛选。
func (s *LogStore) Query(limit int, model, account, status, path string) []LogEntry {
	return s.QueryFilter(limit, LogFilter{Model: model, Account: account, Status: status, Path: path})
}

// QueryFilter 完整筛选。
func (s *LogStore) QueryFilter(limit int, f LogFilter) []LogEntry {
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink != nil {
		if rows, err := sink.QueryLogs(limit, f); err == nil && rows != nil {
			return rows
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.entries)
	out := make([]LogEntry, 0, n)
	for i := n - 1; i >= 0; i-- {
		e := s.entries[i]
		if f.Model != "" && e.Model != f.Model {
			continue
		}
		if f.Account != "" && e.Account != f.Account {
			continue
		}
		if f.Status != "" && !matchStatus(e.Status, f.Status) {
			continue
		}
		if f.Path != "" && !containsFold(e.Path, f.Path) {
			continue
		}
		if f.FailClass != "" && e.FailClass != f.FailClass {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Get 按内存环形 ID 或 PostgreSQL id 取一条。
func (s *LogStore) Get(id int64) *LogEntry {
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink != nil {
		if row, err := sink.GetLog(id); err == nil && row != nil {
			return row
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			cp := s.entries[i]
			return &cp
		}
	}
	return nil
}

// GetByRequestID 按请求 ID 取最近一条。
func (s *LogStore) GetByRequestID(rid string) *LogEntry {
	if strings.TrimSpace(rid) == "" {
		return nil
	}
	rows := s.Query(200, "", "", "", "")
	for i := range rows {
		if rows[i].RequestID == rid {
			cp := rows[i]
			return &cp
		}
	}
	return nil
}

// Stats 汇总统计。
type Stats struct {
	TotalRequests   int            `json:"total_requests"`
	ErrorRequests   int            `json:"error_requests"`
	ErrorRate       float64        `json:"error_rate"`
	TotalPrompt     int            `json:"total_prompt_tokens"`
	TotalCompletion int            `json:"total_completion_tokens"`
	TotalTokens     int            `json:"total_tokens"`
	AvgLatency      int64          `json:"avg_latency_ms"`
	ByAccount       map[string]int `json:"by_account"`
	ByModel         map[string]int `json:"by_model"`
	ByPath          map[string]int `json:"by_path"`
	ByFailClass     map[string]int `json:"by_fail_class"`
	ByClientKey     map[string]int `json:"by_client_key"`
	TokensByAccount map[string]int `json:"tokens_by_account"`
	TokensByModel   map[string]int `json:"tokens_by_model"`
}

// ComputeStats 计算全量统计。
func (s *LogStore) ComputeStats() Stats {
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink != nil {
		if st, err := sink.ComputeLogStats(); err == nil {
			return st
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		ByAccount:       map[string]int{},
		ByModel:         map[string]int{},
		ByPath:          map[string]int{},
		ByFailClass:     map[string]int{},
		ByClientKey:     map[string]int{},
		TokensByAccount: map[string]int{},
		TokensByModel:   map[string]int{},
	}
	var latencySum int64
	for _, e := range s.entries {
		st.TotalRequests++
		latencySum += e.Latency
		// 客户端主动断开（499/canceled）不是服务端错误：不计入错误率。
		// 双条件兜底——status 未落库但 FailClass 落了的行（或反之）都能被剔除。
		if e.Status >= 400 && e.Status != failclass.StatusClientClosedRequest && e.FailClass != string(failclass.Canceled) {
			st.ErrorRequests++
		}
		st.TotalPrompt += e.Prompt
		st.TotalCompletion += e.Completion
		st.TotalTokens += e.Total
		if e.Account != "" {
			st.ByAccount[e.Account]++
			if e.Total != 0 {
				st.TokensByAccount[e.Account] += e.Total
			}
		}
		if e.Model != "" {
			st.ByModel[e.Model]++
			if e.Total != 0 {
				st.TokensByModel[e.Model] += e.Total
			}
		}
		st.ByPath[e.Path]++
		if e.FailClass != "" {
			st.ByFailClass[e.FailClass]++
		}
		if e.ClientKeyID != nil {
			st.ByClientKey[strconv.FormatUint(*e.ClientKeyID, 10)]++
		}
	}
	if st.TotalRequests > 0 {
		st.ErrorRate = float64(st.ErrorRequests) / float64(st.TotalRequests)
		st.AvgLatency = latencySum / int64(st.TotalRequests)
	}
	return st
}

// matchStatus 匹配状态码（支持精确 "200" 或前缀 "5xx" / "4xx"）。
func matchStatus(code int, pattern string) bool {
	if pattern == "4xx" {
		return code >= 400 && code < 500
	}
	if pattern == "5xx" {
		return code >= 500 && code < 600
	}
	if len(pattern) == 3 && pattern[2] == 'x' {
		prefix := int(pattern[0]-'0') * 100
		return code >= prefix && code < prefix+100
	}
	var want int
	for _, c := range pattern {
		if c < '0' || c > '9' {
			return false
		}
		want = want*10 + int(c-'0')
	}
	return code == want
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	return len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
