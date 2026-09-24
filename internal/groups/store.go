// Package groups 把分组提升为一等实体（对照 kiro.rs src/admin/groups.rs）。
//
// 分组落在 persist.Backend（生产是 PostgreSQL），账号与 Client Key 只存名字引用。
// 改名级联账号 groups[]；Key 通过 KeyGroupRenamer 钩子（W2.2 未到也可先 SetKeyRenamer）。
// 组配置 overlay：rpm / daily_max；账号字段 >0 用账号值，否则回退组覆盖。
package groups

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"prism-2api/internal/persist"
)

const maxNameRunes = 64

// Config 是组级 RPM / 日限覆盖。0 表示该字段不覆盖。
type Config struct {
	RPM      int `json:"rpm,omitempty"`
	DailyMax int `json:"daily_max,omitempty"`
}

func (c *Config) empty() bool {
	return c == nil || (c.RPM <= 0 && c.DailyMax <= 0)
}

// Group 是 groups.json 里的一条记录。name 是主键（trim 后、区分大小写）。
type Group struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	CreatedAt   string  `json:"created_at"`
	Config      *Config `json:"config,omitempty"`
}

// KeyGroupRenamer 是 clientkeys 改名级联钩子。
type KeyGroupRenamer interface {
	RenameGroup(old, new string)
}

// AccountGroupRenamer 级联账号 Account.groups[]（由 pool.Pool 实现）。
type AccountGroupRenamer interface {
	RenameGroup(old, new string)
}

// GroupMember 接受 SetGroups（供校验后写入）。
type GroupMember interface {
	SetGroups([]string)
}

// Store 是线程安全的分组注册表，写操作自动落盘。
type Store struct {
	mu       sync.RWMutex
	entries  map[string]Group
	backend  persist.Backend
	accounts AccountGroupRenamer
	keys     KeyGroupRenamer
}

// DefaultPath 是 CredentialDir 下的 groups.json（与 kiro default_path_in 相同）。
func DefaultPath(credentialDir string) string {
	return filepath.Join(credentialDir, "groups.json")
}

// Open 从 Backend 加载分组。
func Open(b persist.Backend) (*Store, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	s := &Store{
		entries: map[string]Group{},
		backend: b,
	}
	data, err := b.LoadDoc(persist.KindGroups, persist.IDAll)
	if err != nil {
		return nil, err
	}
	if err := s.loadJSON(data); err != nil {
		return nil, err
	}
	return s, nil
}

// Load 一次性读入旧 JSON 到内存 Backend（不回写文件）。path 空则空库。
func Load(path string) (*Store, error) {
	mem := persist.NewMemory()
	if err := persist.ImportGroupsFile(mem, path); err != nil {
		return nil, err
	}
	return Open(mem)
}

func (s *Store) loadJSON(b []byte) error {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	var list []Group
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse groups: %w", err)
	}
	for _, g := range list {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			continue
		}
		g.Name = name
		if g.Config.empty() {
			g.Config = nil
		}
		s.entries[name] = g
	}
	return nil
}

// SetKeyRenamer 挂上 Client Key 改名级联（未实现 clientkeys 时可不设）。
func (s *Store) SetKeyRenamer(r KeyGroupRenamer) {
	s.mu.Lock()
	s.keys = r
	s.mu.Unlock()
}

// SetAccountRenamer 挂上账号 groups[] 改名级联。
func (s *Store) SetAccountRenamer(r AccountGroupRenamer) {
	s.mu.Lock()
	s.accounts = r
	s.mu.Unlock()
}

// List 按 name 字典序返回全部组。
func (s *Store) List() []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Group, 0, len(s.entries))
	for _, g := range s.entries {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 按主键取组。
func (s *Store) Get(name string) (Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.entries[strings.TrimSpace(name)]
	return g, ok
}

// Exists 是否已注册。
func (s *Store) Exists(name string) bool {
	_, ok := s.Get(name)
	return ok
}

// Limit 返回该组 overlay；未注册或未配置时 rpm/dailyMax 均为 0（不覆盖）。
func (s *Store) Limit(name string) (rpm, dailyMax int) {
	g, ok := s.Get(name)
	if !ok || g.Config == nil {
		return 0, 0
	}
	return g.Config.RPM, g.Config.DailyMax
}

// Missing 返回 names 里尚未注册的名字（已 trim；空串忽略）。
func (s *Store) Missing(names []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var miss []string
	seen := map[string]struct{}{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := s.entries[n]; ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		miss = append(miss, n)
	}
	return miss
}

// ValidateNames 要求每个非空名字都已注册。
func (s *Store) ValidateNames(names []string) error {
	miss := s.Missing(names)
	if len(miss) == 0 {
		return nil
	}
	return fmt.Errorf("unknown groups: %s", strings.Join(miss, ", "))
}

// SetGroups 校验引用后写入成员（Account.SetGroups 本身不查注册表）。
func (s *Store) SetGroups(m GroupMember, names []string) error {
	cleaned := cleanGroupNames(names)
	if err := s.ValidateNames(cleaned); err != nil {
		return err
	}
	if m != nil {
		m.SetGroups(cleaned)
	}
	return nil
}

// Create 注册新组。重名报错，不覆盖。
func (s *Store) Create(name, description string) (Group, error) {
	trimmed, err := normalizeName(name)
	if err != nil {
		return Group{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[trimmed]; ok {
		return Group{}, fmt.Errorf("group already exists: %s", trimmed)
	}
	g := Group{
		Name:        trimmed,
		Description: cleanDesc(description),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.entries[trimmed] = g
	if err := s.saveLocked(); err != nil {
		delete(s.entries, trimmed)
		return Group{}, err
	}
	return g, nil
}

// Update 改备注和/或整份 config。description/cfg 为 nil 表示该侧不改；
// cfg 非 nil 且字段全 0 则清除 overlay。
func (s *Store) Update(name string, description *string, cfg *Config) (Group, error) {
	key := strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.entries[key]
	if !ok {
		return Group{}, fmt.Errorf("group not found: %s", key)
	}
	if description != nil {
		g.Description = cleanDesc(*description)
	}
	if cfg != nil {
		if cfg.empty() {
			g.Config = nil
		} else {
			cp := *cfg
			g.Config = &cp
		}
	}
	s.entries[key] = g
	if err := s.saveLocked(); err != nil {
		return Group{}, err
	}
	return g, nil
}

// Rename 改主键并级联账号 / Key。新旧名相同（trim 后）视为 no-op。
func (s *Store) Rename(oldName, newName string) (Group, error) {
	oldName = strings.TrimSpace(oldName)
	trimmed, err := normalizeName(newName)
	if err != nil {
		return Group{}, err
	}
	s.mu.Lock()
	g, ok := s.entries[oldName]
	if !ok {
		s.mu.Unlock()
		return Group{}, fmt.Errorf("group not found: %s", oldName)
	}
	if trimmed == oldName {
		s.mu.Unlock()
		return g, nil
	}
	if _, exists := s.entries[trimmed]; exists {
		s.mu.Unlock()
		return Group{}, fmt.Errorf("target group name already exists: %s", trimmed)
	}
	orig := g
	delete(s.entries, oldName)
	g.Name = trimmed
	s.entries[trimmed] = g
	if err := s.saveLocked(); err != nil {
		delete(s.entries, trimmed)
		s.entries[oldName] = orig
		s.mu.Unlock()
		return Group{}, err
	}
	accounts := s.accounts
	keys := s.keys
	s.mu.Unlock()
	if accounts != nil {
		accounts.RenameGroup(oldName, trimmed)
	}
	if keys != nil {
		keys.RenameGroup(oldName, trimmed)
	}
	return g, nil
}

// Delete 删除注册表项。返回是否确实删了。不级联清账号引用（与 kiro 默认删一致）。
func (s *Store) Delete(name string) bool {
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[name]; !ok {
		return false
	}
	delete(s.entries, name)
	_ = s.saveLocked()
	return true
}

func (s *Store) saveLocked() error {
	if s.backend == nil {
		return nil
	}
	list := make([]Group, 0, len(s.entries))
	for _, g := range s.entries {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.backend.SaveDoc(persist.KindGroups, persist.IDAll, b)
}

func normalizeName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("group name is empty")
	}
	if utf8.RuneCountInString(trimmed) > maxNameRunes {
		return "", fmt.Errorf("group name too long (max %d)", maxNameRunes)
	}
	return trimmed, nil
}

func cleanDesc(s string) string {
	return strings.TrimSpace(s)
}

func cleanGroupNames(names []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
