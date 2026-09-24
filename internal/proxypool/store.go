// Package proxypool 管理往 Cursor 发请求的出口代理（以 Resin 反代为主，也可挂 HTTP 正代）。
package proxypool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/egress"
	"prism-2api/internal/persist"
)

const kindDoc = persist.KindProxies

// Store 出口池。
type Store struct {
	mu      sync.Mutex
	items   map[string]Proxy
	backend persist.Backend
}

// Open 从 Backend 加载。
func Open(b persist.Backend) (*Store, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	s := &Store{items: map[string]Proxy{}, backend: b}
	data, err := b.LoadDoc(kindDoc, persist.IDAll)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return s, nil
	}
	var list []Proxy
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, p := range list {
		if p.ID == "" {
			continue
		}
		s.items[p.ID] = p
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	if s.backend == nil {
		return nil
	}
	list := s.listLocked()
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.backend.SaveDoc(kindDoc, persist.IDAll, b)
}

func (s *Store) listLocked() []Proxy {
	out := make([]Proxy, 0, len(s.items))
	for _, p := range s.items {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// List 全部出口。
func (s *Store) List() []Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// Get 按 ID。
func (s *Store) Get(id string) (Proxy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	return p, ok
}

// Default 返回标记为默认且启用的出口。
func (s *Store) Default() (Proxy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.items {
		if p.IsDefault && p.Enabled {
			return p, true
		}
	}
	return Proxy{}, false
}

// Create 新建。
func (s *Store) Create(req CreateRequest) (Proxy, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Proxy{}, fmt.Errorf("name is required")
	}
	kind := normalizeKind(req.Kind)
	if kind == "" {
		return Proxy{}, fmt.Errorf("kind must be resin, http, or direct")
	}
	if kind == egress.KindResin && strings.TrimSpace(req.ResinURL) == "" {
		return Proxy{}, fmt.Errorf("resin_url is required")
	}
	if kind == egress.KindHTTP && strings.TrimSpace(req.HTTPProxy) == "" {
		return Proxy{}, fmt.Errorf("http_proxy is required")
	}
	now := time.Now()
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	p := Proxy{
		ID:        newID(),
		Name:      name,
		Kind:      kind,
		Enabled:   enabled,
		IsDefault: req.IsDefault,
		Notes:     strings.TrimSpace(req.Notes),
		HTTPProxy: strings.TrimSpace(req.HTTPProxy),
		ResinURL:  strings.TrimRight(strings.TrimSpace(req.ResinURL), "/"),
		ResinPlat: strings.TrimSpace(req.ResinPlat),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if p.ResinPlat == "" {
		p.ResinPlat = egress.DefaultPlatform
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.IsDefault {
		s.clearDefaultLocked()
	}
	s.items[p.ID] = p
	if err := s.saveLocked(); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

// Patch 部分更新。
func (s *Store) Patch(id string, req PatchRequest) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return Proxy{}, fmt.Errorf("proxy not found")
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			return Proxy{}, fmt.Errorf("name is required")
		}
		p.Name = n
	}
	if req.Kind != nil {
		k := normalizeKind(*req.Kind)
		if k == "" {
			return Proxy{}, fmt.Errorf("kind must be resin, http, or direct")
		}
		p.Kind = k
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	if req.Notes != nil {
		p.Notes = strings.TrimSpace(*req.Notes)
	}
	if req.HTTPProxy != nil {
		p.HTTPProxy = strings.TrimSpace(*req.HTTPProxy)
	}
	if req.ResinURL != nil {
		p.ResinURL = strings.TrimRight(strings.TrimSpace(*req.ResinURL), "/")
	}
	if req.ResinPlat != nil {
		p.ResinPlat = strings.TrimSpace(*req.ResinPlat)
		if p.ResinPlat == "" {
			p.ResinPlat = egress.DefaultPlatform
		}
	}
	if req.IsDefault != nil {
		p.IsDefault = *req.IsDefault
		if p.IsDefault {
			s.clearDefaultLocked()
		}
	}
	p.UpdatedAt = time.Now()
	s.items[id] = p
	if err := s.saveLocked(); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

// Delete 删除。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return fmt.Errorf("proxy not found")
	}
	delete(s.items, id)
	return s.saveLocked()
}

// RememberProbe 写入探测结果。
func (s *Store) RememberProbe(id, ip, errMsg string) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return Proxy{}, fmt.Errorf("proxy not found")
	}
	p.LastProbeIP = ip
	p.LastError = errMsg
	p.LastProbeAt = time.Now().Unix()
	p.UpdatedAt = time.Now()
	s.items[id] = p
	if err := s.saveLocked(); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

func (s *Store) clearDefaultLocked() {
	for id, p := range s.items {
		if p.IsDefault {
			p.IsDefault = false
			s.items[id] = p
		}
	}
}

func normalizeKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case egress.KindResin, "reverse", "reverse-proxy":
		return egress.KindResin
	case egress.KindHTTP, "https", "http-proxy":
		return egress.KindHTTP
	case egress.KindDirect, "none", "off":
		return egress.KindDirect
	default:
		return ""
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "px_" + hex.EncodeToString(b[:])
}

// SettingsFor 把一条出口 + 账号粘性标识变成 egress.Settings。
func SettingsFor(p Proxy, accountID string) egress.Settings {
	s := egress.Settings{Kind: p.Kind, Account: accountID}
	switch p.Kind {
	case egress.KindHTTP:
		s.HTTPProxyURL = p.HTTPProxy
	case egress.KindResin:
		s.ResinURL = p.ResinURL
		s.ResinPlatform = p.ResinPlat
		if s.ResinPlatform == "" {
			s.ResinPlatform = egress.DefaultPlatform
		}
	}
	return s
}
