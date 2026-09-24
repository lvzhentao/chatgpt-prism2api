package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Store 负责凭据的持久化（JSON 文件，原子写入）。
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore 创建凭据存储。
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Save 保存令牌到文件。
func (s *Store) Save(t *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Load 从文件加载令牌；不存在时返回 ErrNoCredentials。
var ErrNoCredentials = errors.New("no credentials saved")

func (s *Store) Load() (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, ErrNoCredentials
	}
	return &t, nil
}

// Clear 删除凭据文件。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
