// Package persist 是唯一持久化后端接口。生产实现是 PostgreSQL；测试用内存。
// 不再双写 JSON 文件。
package persist

import (
	"sync"
)

const (
	KindConfig     = "config"
	KindAccount    = "account"
	KindClientKeys = "client_keys"
	KindGroups     = "groups"
	KindProxies    = "proxies"
	KindTasks      = "tasks"
	KindEmulation  = "emulation"

	IDMain = "main"
	IDAll  = "all"
)

// Backend 文档型存储（kind + id → JSON）。
type Backend interface {
	LoadDoc(kind, id string) ([]byte, error)
	SaveDoc(kind, id string, payload []byte) error
	DeleteDoc(kind, id string) error
	ListDocs(kind string) (map[string][]byte, error)
}

// Memory 进程内 Backend，供测试与 --mock 无库启动。
type Memory struct {
	mu   sync.RWMutex
	docs map[string]map[string][]byte
}

// NewMemory 空内存库。
func NewMemory() *Memory {
	return &Memory{docs: map[string]map[string][]byte{}}
}

func (m *Memory) LoadDoc(kind, id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.docs[kind][id]
	if b == nil {
		return nil, nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (m *Memory) SaveDoc(kind, id string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docs[kind] == nil {
		m.docs[kind] = map[string][]byte{}
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	m.docs[kind][id] = cp
	return nil
}

func (m *Memory) DeleteDoc(kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docs[kind] != nil {
		delete(m.docs[kind], id)
	}
	return nil
}

func (m *Memory) ListDocs(kind string) (map[string][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.docs[kind]
	out := make(map[string][]byte, len(src))
	for k, v := range src {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out, nil
}
