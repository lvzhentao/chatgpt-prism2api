package scheduler

import (
	"sync"
	"time"
)

// sessionCache 是 CPA SessionCache 的精简版：TTL 映射 + 多标识 alias + InvalidateAuth。
// 对照 sdk/cliproxy/auth/session_cache.go，不搬 prompt-cache alias 压缩特例。
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
	ttl     time.Duration
}

type sessionEntry struct {
	authID    string
	expiresAt time.Time
	aliases   []string
}

func newSessionCache(ttl time.Duration) *sessionCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &sessionCache{entries: make(map[string]sessionEntry), ttl: ttl}
}

func (c *sessionCache) setTTL(ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl > 0 {
		c.ttl = ttl
	}
}

func (c *sessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[sessionID]
	if !ok || !now.Before(entry.expiresAt) {
		if ok {
			c.removeGroupLocked(entry)
		}
		return "", false
	}
	return entry.authID, true
}

func (c *sessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[sessionID]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		c.removeGroupLocked(entry)
		return "", false
	}
	aliases := mergeAliases([]string{sessionID}, entry.aliases...)
	c.replaceLocked(entry.authID, now.Add(c.ttl), aliases, entry)
	return entry.authID, true
}

func (c *sessionCache) SetAliases(authID string, sessionIDs ...string) {
	if authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	aliases := mergeAliases(nil, sessionIDs...)
	var previous []sessionEntry
	for _, id := range sessionIDs {
		entry, ok := c.entries[id]
		if !ok {
			continue
		}
		if !now.Before(entry.expiresAt) {
			c.removeGroupLocked(entry)
			continue
		}
		previous = append(previous, entry)
		aliases = mergeAliases(aliases, entry.aliases...)
	}
	if len(aliases) == 0 {
		return
	}
	c.replaceLocked(authID, now.Add(c.ttl), aliases, previous...)
}

func (c *sessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var groups []sessionEntry
	seen := map[string]struct{}{}
	for _, entry := range c.entries {
		if entry.authID != authID {
			continue
		}
		key := entry.authID + "|" + entry.expiresAt.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		groups = append(groups, entry)
	}
	for _, g := range groups {
		c.removeGroupLocked(g)
	}
}

func (c *sessionCache) replaceLocked(authID string, expiresAt time.Time, aliases []string, previous ...sessionEntry) {
	for _, p := range previous {
		c.removeGroupLocked(p)
	}
	entry := sessionEntry{authID: authID, expiresAt: expiresAt, aliases: aliases}
	for _, alias := range aliases {
		c.entries[alias] = entry
	}
}

func (c *sessionCache) removeGroupLocked(entry sessionEntry) {
	for _, alias := range entry.aliases {
		cur, ok := c.entries[alias]
		if !ok || cur.authID != entry.authID {
			continue
		}
		delete(c.entries, alias)
	}
}

func mergeAliases(existing []string, candidates ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(existing)+len(candidates))
	for _, id := range append(existing, candidates...) {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
