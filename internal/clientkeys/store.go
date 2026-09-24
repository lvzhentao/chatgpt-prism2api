package clientkeys

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/persist"
)

// Store 客户端 Key 管理器。
//
// 内部双索引（对齐 kiro ClientKeyManager）：
//   - byKey: 明文 → id，O(1) 查重 / 删除
//   - entries: id → Key，按 id 读写明细
//
// 鉴权比对仍扫描全部未禁用 Key 并走 Equal（常量时间），不用 byKey 短路。
type Store struct {
	mu      sync.RWMutex
	entries map[uint64]*Key
	byKey   map[string]uint64
	nextID  uint64
	backend persist.Backend
	// usageDirty 自上次落盘以来未持久化的 RecordUsage 次数；达到阈值批量落盘。
	usageDirty int
}

// usageFlushThreshold 是 RecordUsage 合并写的计数阈值：每 N 次用量累计落盘一次。
// 选计数阈值而非 5s ticker：Store 无后台循环、无 Shutdown 钩子，加 goroutine 要管
// 生命周期（泄漏风险），而请求完成路径本就是唯一写者，计数阈值零后台成本；
// 管理类变更（Create/Delete 等）仍同步落盘，只有高频用量走合并。
const usageFlushThreshold = 100

func emptyStore(b persist.Backend) *Store {
	if b == nil {
		b = persist.NewMemory()
	}
	return &Store{
		entries: make(map[uint64]*Key),
		byKey:   make(map[string]uint64),
		nextID:  1,
		backend: b,
	}
}

// New 空的内存管理器。
func New() *Store {
	s, _ := Open(persist.NewMemory())
	return s
}

// Open 从 Backend 加载。
func Open(b persist.Backend) (*Store, error) {
	s := emptyStore(b)
	data, err := s.backend.LoadDoc(persist.KindClientKeys, persist.IDAll)
	if err != nil {
		return nil, err
	}
	if err := s.loadJSON(data); err != nil {
		return nil, err
	}
	return s, nil
}

// PathIn 返回凭据目录下的 client_api_keys.json。
func PathIn(dir string) string {
	return filepath.Join(dir, FileName)
}

// Load 一次性读入旧 JSON 到内存 Backend（不回写文件）。
func Load(path string) (*Store, error) {
	mem := persist.NewMemory()
	if err := persist.ImportClientKeysFile(mem, path); err != nil {
		return nil, err
	}
	return Open(mem)
}

func (s *Store) loadJSON(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var list []Key
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	var maxID uint64
	for i := range list {
		k := list[i]
		if k.ID > maxID {
			maxID = k.ID
		}
		cp := k
		s.entries[k.ID] = &cp
		s.byKey[k.Key] = k.ID
	}
	s.nextID = maxID + 1
	return nil
}

// saveLocked 全量序列化并写 Backend（须持 s.mu 写锁）。
func (s *Store) saveLocked() {
	if s.backend == nil {
		return
	}
	list := make([]Key, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, *e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	_ = s.backend.SaveDoc(persist.KindClientKeys, persist.IDAll, data)
	s.usageDirty = 0
}

// Flush 把内存态（含未落盘的用量累计）同步刷到 Backend，供测试与退出路径调用。
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveLocked()
}

// Get 按 id 返回副本。
func (s *Store) Get(id uint64) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return Key{}, false
	}
	return *e, true
}

// List 按 id 升序返回副本。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]Key, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, *e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Create 生成 csk_* 明文并入库。
func (s *Store) Create(name, description, group string) Key {
	return s.CreateWithKey(name, description, group, Generate())
}

// CreateWithKey 用指定明文创建（bootstrap / 测试）。明文已存在则返回已有条目。
func (s *Store) CreateWithKey(name, description, group, plaintext string) Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byKey[plaintext]; ok {
		return *s.entries[id]
	}
	id := s.nextID
	s.nextID++
	now := time.Now().UTC().Format(time.RFC3339)
	e := &Key{
		ID:          id,
		Key:         plaintext,
		Name:        name,
		Description: description,
		CreatedAt:   now,
		Group:       strings.TrimSpace(group),
	}
	s.byKey[plaintext] = id
	s.entries[id] = e
	s.saveLocked()
	return *e
}

// EnsureSystemKey 确保 runtime api_key_auth 对应的系统 Key 存在（幂等，对齐 kiro ensure_system_key）。
//
// 系统 Key 固定占用 id=0（历史 master 用量记在 keyId=0）。不可删除。
func (s *Store) EnsureSystemKey(name, description, plaintext string) {
	if plaintext == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byKey[plaintext]; ok {
		switch id {
		case 0:
			if e := s.entries[0]; e != nil && !e.IsSystem {
				e.IsSystem = true
				s.saveLocked()
			}
		default:
			if _, taken := s.entries[0]; !taken {
				e := s.entries[id]
				delete(s.entries, id)
				e.ID = 0
				e.IsSystem = true
				s.entries[0] = e
				s.byKey[plaintext] = 0
				s.saveLocked()
				return
			}
			if e := s.entries[id]; e != nil && !e.IsSystem {
				e.IsSystem = true
				s.saveLocked()
			}
		}
		return
	}
	var id uint64
	if _, taken := s.entries[0]; !taken {
		id = 0
	} else {
		id = s.nextID
		s.nextID++
	}
	e := &Key{
		ID:          id,
		Key:         plaintext,
		Name:        name,
		Description: description,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		IsSystem:    true,
	}
	s.byKey[plaintext] = id
	s.entries[id] = e
	s.saveLocked()
}

// Delete 删除；系统 Key 拒绝删除。
func (s *Store) Delete(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || e.IsSystem {
		return false
	}
	delete(s.entries, id)
	delete(s.byKey, e.Key)
	s.saveLocked()
	return true
}

// SetDisabled 启用/禁用。
func (s *Store) SetDisabled(id uint64, disabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return false
	}
	e.Disabled = disabled
	s.saveLocked()
	return true
}

// UpdateMeta 更新名称 / 描述 / 分组（空 group 表示解绑）。
func (s *Store) UpdateMeta(id uint64, name *string, description *string, group *string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return false
	}
	if name != nil {
		e.Name = *name
	}
	if description != nil {
		e.Description = *description
	}
	if group != nil {
		e.Group = strings.TrimSpace(*group)
	}
	s.saveLocked()
	return true
}

// GroupOf 返回绑定分组；未绑定或不存在为 ""。
func (s *Store) GroupOf(id uint64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.entries[id]; e != nil {
		return e.Group
	}
	return ""
}

// IsSystem 指定 id 是否为系统 Key。
func (s *Store) IsSystem(id uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.entries[id]; e != nil {
		return e.IsSystem
	}
	return false
}

// RenameGroup 把引用 old 的 Key.group 改为 new（分组改名级联）。
func (s *Store) RenameGroup(old, newName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.Group == old {
			e.Group = newName
			n++
		}
	}
	if n > 0 {
		s.saveLocked()
	}
	return n
}

// ClearGroup 清空引用 name 的 group（强删分组级联）。
func (s *Store) ClearGroup(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if e.Group == name {
			e.Group = ""
			n++
		}
	}
	if n > 0 {
		s.saveLocked()
	}
	return n
}

// Rotate 轮换明文，保留 id / 元数据 / 统计 / is_system。
func (s *Store) Rotate(id uint64) (Key, bool) {
	newKey := Generate()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return Key{}, false
	}
	delete(s.byKey, e.Key)
	e.Key = newKey
	s.byKey[newKey] = id
	s.saveLocked()
	return *e, true
}

// ResetStats 清零计数。
func (s *Store) ResetStats(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return false
	}
	e.TotalCalls = 0
	e.TotalInputTokens = 0
	e.TotalOutputTokens = 0
	e.TotalCacheCreationTokens = 0
	e.TotalCacheReadTokens = 0
	e.TotalCredits = 0
	s.saveLocked()
	return true
}

// VerifyAndTouch 校验明文。命中且未禁用则返回条目（更新 last_used / total_calls，不落盘）。
//
// 对所有未禁用 Key 做 Equal（不 break），防止时序攻击。不做前缀检查：系统 Key 可以是任意格式。
func (s *Store) VerifyAndTouch(presented string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hitID uint64
	found := false
	for id, ck := range s.entries {
		if ck.Disabled {
			continue
		}
		if Equal(ck.Key, presented) {
			hitID = id
			found = true
			// 不 break，扫完全部以保持常量时间
		}
	}
	if !found {
		return Key{}, false
	}
	e := s.entries[hitID]
	e.TotalCalls++
	e.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	return *e, true
}

// RecordUsage 累计 Token 到内存，每 usageFlushThreshold 次落盘一次。
//
// 字段累加口径与此前同步落盘版本一致；冷却决策仍在内存立即生效，异步的只是
// 落盘。崩溃最多丢 usageFlushThreshold-1 次用量计数（纯计数风险，无冷却语义，
// 与 T3.1 同一风险口径）。测试与退出路径调 Flush() 同步刷盘。
// VerifyAndTouch 的 TotalCalls++ 本来就不落盘，此处不对齐改动它。
func (s *Store) RecordUsage(id, input, output, cacheCreate, cacheRead uint64, credits float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return
	}
	e.TotalInputTokens += input
	e.TotalOutputTokens += output
	e.TotalCacheCreationTokens += cacheCreate
	e.TotalCacheReadTokens += cacheRead
	if credits > 0 {
		e.TotalCredits += credits
	}
	e.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	s.usageDirty++
	if s.usageDirty >= usageFlushThreshold {
		s.saveLocked()
	}
}

// ActiveCount 未禁用 Key 数。
func (s *Store) ActiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.entries {
		if !e.Disabled {
			n++
		}
	}
	return n
}
