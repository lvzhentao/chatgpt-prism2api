package emulation

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/persist"
	"prism-2api/internal/secret"
)

const (
	maxRecentLogs = 20
	maxTavilyKeys = 32
)

// Store 持久化协议外观配置，并持有进程内缓存 tracker / 最近日志。
type Store struct {
	mu      sync.RWMutex
	cfg     Config
	backend persist.Backend
	tracker *Tracker

	tavilyRR   uint64
	cacheLogs  []CacheLog
	searchLogs []SearchLog
}

// SearchLog 是最近一次 web_search 拦截记录。
type SearchLog struct {
	Time      string `json:"time"`
	Query     string `json:"query"`
	Provider  string `json:"provider"`
	Results   int    `json:"results"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// Open 从 persist 加载配置。
func Open(b persist.Backend) (*Store, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	s := &Store{
		cfg:     DefaultConfig(),
		backend: b,
		tracker: NewTracker(),
	}
	data, err := b.LoadDoc(persist.KindEmulation, persist.IDMain)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		var loaded Config
		if err := json.Unmarshal(data, &loaded); err != nil {
			return nil, fmt.Errorf("parse emulation config: %w", err)
		}
		s.cfg = loaded.normalize()
	}
	return s, nil
}

// Config 返回当前配置副本。
func (s *Store) Config() Config {
	if s == nil {
		return DefaultConfig()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 用客户端提交覆盖配置（masked Tavily key 保留原密文）。
func (s *Store) Update(next Config) (Config, error) {
	if s == nil {
		return Config{}, fmt.Errorf("emulation store not ready")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := next.normalize()
	merged.WebSearch.TavilyKeys = mergeTavilyKeys(s.cfg.WebSearch.TavilyKeys, next.WebSearch.TavilyKeys)
	s.cfg = merged
	if err := s.saveLocked(); err != nil {
		return Config{}, err
	}
	return s.cfg.PublicView(), nil
}

func (s *Store) saveLocked() error {
	raw, err := json.Marshal(s.cfg)
	if err != nil {
		return err
	}
	return s.backend.SaveDoc(persist.KindEmulation, persist.IDMain, raw)
}

func mergeTavilyKeys(existing, incoming []TavilyKey) []TavilyKey {
	byID := map[string]TavilyKey{}
	for _, k := range existing {
		if k.ID != "" {
			byID[k.ID] = k
		}
	}
	out := make([]TavilyKey, 0, len(incoming))
	for _, k := range incoming {
		k.Name = strings.TrimSpace(k.Name)
		k.Key = strings.TrimSpace(k.Key)
		if k.ID == "" {
			k.ID = "tvk_" + randomID(10)
		}
		if prev, ok := byID[k.ID]; ok {
			if looksMasked(k.Key) {
				k.Key = prev.Key
			}
			if k.LastError == "" {
				k.LastError = prev.LastError
			}
			if k.LastUsedAt == "" {
				k.LastUsedAt = prev.LastUsedAt
			}
		}
		if !looksMasked(k.Key) && k.Key != "" && !strings.HasPrefix(k.Key, "enc:v1:") {
			k.Key = secret.Seal(k.Key)
		}
		if k.Key == "" && !k.Enabled {
			continue
		}
		out = append(out, k)
		if len(out) >= maxTavilyKeys {
			break
		}
	}
	return out
}

// PrepareCache 估算本次请求的缓存拆分。
func (s *Store) PrepareCache(ns uint64, body []byte, model string, inputTokens int) *Plan {
	if s == nil {
		return nil
	}
	cfg := s.Config()
	if !cfg.Cache.Enabled {
		return nil
	}
	return s.tracker.prepare(cfg.Cache, ns, body, model, inputTokens)
}

// NoteCache 记下最近一次估算。
func (s *Store) NoteCache(model string, input int, usage *Usage) {
	if s == nil || usage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheLogs = append([]CacheLog{{
		Time:     time.Now().Format(time.RFC3339),
		Model:    model,
		Total:    input,
		Input:    usage.InputTokens,
		Read:     usage.CacheReadInputTokens,
		Creation: usage.CacheCreationInputTokens,
		Hit:      usage.CacheReadInputTokens > 0,
	}}, s.cacheLogs...)
	if len(s.cacheLogs) > maxRecentLogs {
		s.cacheLogs = s.cacheLogs[:maxRecentLogs]
	}
}

// NoteSearch 记下最近一次 web_search。
func (s *Store) NoteSearch(log SearchLog) {
	if s == nil {
		return
	}
	if log.Time == "" {
		log.Time = time.Now().Format(time.RFC3339)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchLogs = append([]SearchLog{log}, s.searchLogs...)
	if len(s.searchLogs) > maxRecentLogs {
		s.searchLogs = s.searchLogs[:maxRecentLogs]
	}
}

// CacheLogs 返回最近缓存记录。
func (s *Store) CacheLogs() []CacheLog {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CacheLog, len(s.cacheLogs))
	copy(out, s.cacheLogs)
	return out
}

// SearchLogs 返回最近搜索记录。
func (s *Store) SearchLogs() []SearchLog {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SearchLog, len(s.searchLogs))
	copy(out, s.searchLogs)
	return out
}

// SignatureEnabled 是否输出 signature_delta。
func (s *Store) SignatureEnabled() bool {
	if s == nil {
		return true
	}
	return s.Config().Signature.Enabled
}

// WebSearchEnabled 是否拦截单工具 web_search。
func (s *Store) WebSearchEnabled() bool {
	if s == nil {
		return false
	}
	return s.Config().WebSearch.Enabled
}

// HasTavily 是否有可用的 Tavily key。
func (s *Store) HasTavily() bool {
	return len(s.enabledTavilyKeys()) > 0
}

func (s *Store) markTavily(id, errMsg string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Format(time.RFC3339)
	for i, k := range s.cfg.WebSearch.TavilyKeys {
		if k.ID != id {
			continue
		}
		s.cfg.WebSearch.TavilyKeys[i].LastUsedAt = now
		s.cfg.WebSearch.TavilyKeys[i].LastError = errMsg
		_ = s.saveLocked()
		return
	}
}

func (s *Store) enabledTavilyKeys() []TavilyKey {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TavilyKey
	for _, k := range s.cfg.WebSearch.TavilyKeys {
		if k.Enabled && strings.TrimSpace(secret.Open(k.Key)) != "" {
			out = append(out, k)
		}
	}
	return out
}
