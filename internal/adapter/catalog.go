package adapter

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultCacheTTL = 10 * time.Minute

type memoryCatalog struct {
	client func() Client

	mu        sync.Mutex
	infos     []*ModelInfo
	err       error
	fetchedAt time.Time
	cacheTTL  time.Duration
	static    []*ModelInfo
}

func newMemoryCatalog(static []*ModelInfo) *memoryCatalog {
	return &memoryCatalog{cacheTTL: defaultCacheTTL, static: static}
}

func (mc *memoryCatalog) SetCacheTTL(d time.Duration) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if d > 0 {
		mc.cacheTTL = d
	}
}

func (mc *memoryCatalog) Invalidate() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.infos = nil
	mc.err = nil
}

func (mc *memoryCatalog) SetClientProvider(fn func() Client) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if fn != nil {
		mc.client = fn
	}
}

func (mc *memoryCatalog) Load() ([]*ModelInfo, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.infos != nil && time.Since(mc.fetchedAt) < mc.cacheTTL {
		return mc.infos, nil
	}
	if mc.infos != nil && mc.err == nil {
		infos := mc.infos
		go mc.refresh()
		return infos, nil
	}
	return mc.loadLocked()
}

func (mc *memoryCatalog) refresh() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	_, _ = mc.loadLocked()
}

func (mc *memoryCatalog) Refresh() ([]*ModelInfo, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.infos = nil
	return mc.loadLocked()
}

func (mc *memoryCatalog) loadLocked() ([]*ModelInfo, error) {
	if mc.client != nil {
		if c := mc.client(); c != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			list, err := c.ListModels(ctx)
			if err == nil && len(list) > 0 {
				out := make([]*ModelInfo, len(list))
				for i := range list {
					m := list[i]
					out[i] = &m
				}
				mc.infos = out
				mc.err = nil
				mc.fetchedAt = time.Now()
				return mc.infos, nil
			}
			mc.err = err
		}
	}
	if len(mc.static) > 0 {
		mc.infos = mc.static
		mc.err = nil
		mc.fetchedAt = time.Now()
		return mc.infos, nil
	}
	if mc.err != nil {
		return nil, mc.err
	}
	return nil, nil
}

// thinkingSuffixes 把客户端模型名后缀还原成档位。xhigh 是实验档：放开侧透传 xhigh，
// 不再静默夹成 high；max 暂归一到 xhigh（仍带 Max 标记）。
// TODO(Main 生产实测)：若上游 400 拒收 xhigh，回退时把 -xhigh/-max 两行的 Level 改回 "high" 即可。
var thinkingSuffixes = []struct {
	Suffix string
	Level  string
	Max    bool
}{
	{"-max", "xhigh", true},
	{"-xhigh", "xhigh", false},
	{"-high", "high", false},
	{"-medium", "medium", false},
	{"-low", "low", false},
}

// ParseModelID splits a model id into base name + thinking suffix.
func ParseModelID(id string) (base, level string, max bool) {
	return parseModelID(id)
}

func parseModelID(id string) (base, level string, max bool) {
	search := id
	if strings.HasSuffix(search, "-fast") {
		search = strings.TrimSuffix(search, "-fast")
	}
	for _, s := range thinkingSuffixes {
		if strings.HasSuffix(search, s.Suffix) {
			return strings.TrimSuffix(search, s.Suffix), s.Level, s.Max
		}
	}
	return id, "", false
}

func (mc *memoryCatalog) Resolve(id string) (string, string, bool) {
	base, level, max := parseModelID(id)
	if infos, err := mc.Load(); err == nil {
		for _, m := range infos {
			if m.ID == id {
				return firstNonEmpty(m.ServerModelName, id), "", m.MaxMode
			}
		}
		for _, m := range infos {
			if m.ID == base {
				server := firstNonEmpty(m.ServerModelName, base)
				if level == "" && m.ThinkingLevel != "" {
					level = m.ThinkingLevel
					max = m.MaxMode
				}
				return server, level, max
			}
			for _, a := range m.Aliases {
				if a == base || a == id {
					return firstNonEmpty(m.ServerModelName, base), level, max
				}
			}
		}
	}
	return base, level, max
}

func (mc *memoryCatalog) ResolveWithThinking(id string, thinkingEnabled bool, effort string) (string, string, bool) {
	server, level, max := mc.Resolve(id)
	if !thinkingEnabled {
		return server, "", max
	}
	if level == "" {
		switch strings.ToLower(effort) {
		case "low":
			level = "low"
		case "high":
			level = "high"
		// xhigh 是实验档：放开侧透传，不再静默夹成 high；max 暂归一到 xhigh（仍带 Max 标记）。
		// TODO(Main 生产实测)：若上游 400 拒收 xhigh，回退时把下面两支 case 改回 level = "high" 即可。
		case "xhigh":
			level = "xhigh"
		case "max":
			level = "xhigh"
			max = true
		default:
			level = "medium"
		}
	}
	return server, level, max
}

func (mc *memoryCatalog) ids(strip bool) []string {
	infos, err := mc.Load()
	if err != nil || infos == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(infos))
	for _, m := range infos {
		id := m.ID
		if strip {
			id = stripVariant(id)
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (mc *memoryCatalog) BareModelIDs() []string   { return mc.ids(true) }
func (mc *memoryCatalog) ModelIDs() []string       { return mc.ids(false) }
func (mc *memoryCatalog) PublicModelIDs() []string { return mc.ids(false) }

func stripVariant(id string) string {
	suffixes := []string{
		"-thinking-xhigh", "-thinking-high", "-thinking-max",
		"-thinking-medium", "-thinking-low", "-thinking",
		"-extra-high", "-max", "-xhigh", "-high", "-medium", "-low", "-none", "-fast",
	}
	for {
		changed := false
		for _, suf := range suffixes {
			if strings.HasSuffix(id, suf) {
				id = strings.TrimSuffix(id, suf)
				changed = true
				break
			}
		}
		if !changed {
			return id
		}
	}
}

// OfficializeModelID is a no-op hook; sites may wrap IDs before exposing /v1/models.
func OfficializeModelID(id string) string { return id }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
