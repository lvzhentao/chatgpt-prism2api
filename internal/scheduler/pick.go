package scheduler

import (
	"net/http"
	"sync"
	"time"

	"prism-2api/internal/pool"
)

// Request 是一次选号输入。Tried 是 CPA conductor 的 tried 集合（本请求已失败的号）。
type Request struct {
	Model   string
	Headers http.Header
	Body    []byte
	Tried   map[string]struct{}
	Group   string
	// FallbackAffinity 是客户端没给任何会话标识时的兜底亲和键（调用方按 API Key 生成）。
	// 热沙箱缓存只对「同一个账号」有效：不粘的话每个请求都可能落到刚被轮询到的冷账号上，
	// 于是每轮都要重付一遍上游 7~35s 的沙箱预热（真机实测）。
	FallbackAffinity string
}

// LimitResolver 由 groups.Store 注入：组覆盖 RPM/日限（账号字段为 0 时生效）。
type LimitResolver func(groups []string) (rpm, dailyMax int)

// Scheduler 对应 CPA SessionAffinitySelector + 策略 selector，单上游 Cursor。
type Scheduler struct {
	mu     sync.Mutex
	cfg    Config
	cache  *sessionCache
	rr     roundRobinState
	weight weightedState
	limits LimitResolver
}

func New(cfg Config) *Scheduler {
	cfg = cfg.normalized()
	return &Scheduler{
		cfg:   cfg,
		cache: newSessionCache(cfg.SessionAffinityTTL),
	}
}

func (s *Scheduler) SetConfig(cfg Config) {
	cfg = cfg.normalized()
	s.mu.Lock()
	s.cfg = cfg
	if s.cache == nil {
		s.cache = newSessionCache(cfg.SessionAffinityTTL)
	} else {
		s.cache.setTTL(cfg.SessionAffinityTTL)
	}
	s.mu.Unlock()
}

func (s *Scheduler) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Scheduler) Invalidate(accountID string) {
	s.mu.Lock()
	cache := s.cache
	s.mu.Unlock()
	if cache != nil {
		cache.InvalidateAuth(accountID)
	}
}

// SetLimitResolver 设置组 overlay 解析（collectReady 在账号 rpm/daily_max==0 时使用）。
func (s *Scheduler) SetLimitResolver(fn LimitResolver) {
	s.mu.Lock()
	s.limits = fn
	s.mu.Unlock()
}

// Pick 对应 CPA SessionAffinitySelector.Pick：
// 已绑定且仍可用 → 即使用更高 priority 恢复也钉住；否则在最高 priority 桶内按策略选。
func (s *Scheduler) Pick(accounts []*pool.Account, req Request) (*pool.Account, error) {
	s.mu.Lock()
	cfg := s.cfg
	cache := s.cache
	limits := s.limits
	s.mu.Unlock()

	now := time.Now()
	candidates := collectReady(accounts, req, cfg, now, limits)
	if len(candidates) == 0 {
		return nil, unavailableFrom(accounts, req, cfg, now)
	}

	if cfg.Strategy == StrategyWeighted {
		candidates = positiveWeight(candidates)
		if len(candidates) == 0 {
			return nil, unavailableFrom(accounts, req, cfg, now)
		}
	}

	primary, fallback := "", ""
	if cfg.SessionAffinity {
		primary, fallback = ExtractSessionIDs(req.Headers, req.Body)
		if primary == "" {
			// 客户端没给会话标识：按 API Key 兜底，让同一个调用方粘住同一个账号。
			primary = req.FallbackAffinity
		}
	}

	bind := func(acc *pool.Account) *pool.Account {
		if acc == nil || !cfg.SessionAffinity || primary == "" || cache == nil {
			return acc
		}
		if fallback != "" && fallback != primary {
			cache.SetAliases(acc.ID, cacheKey(primary, req.Model), cacheKey(fallback, req.Model))
		} else {
			cache.SetAliases(acc.ID, cacheKey(primary, req.Model))
		}
		return acc
	}

	if cfg.SessionAffinity && primary != "" && cache != nil {
		if id, ok := cache.GetAndRefresh(cacheKey(primary, req.Model)); ok {
			if acc := findID(candidates, id); acc != nil {
				return bind(acc), nil
			}
			s.Invalidate(id)
		}
		if fallback != "" && fallback != primary {
			if id, ok := cache.Get(cacheKey(fallback, req.Model)); ok {
				if acc := findID(candidates, id); acc != nil {
					return bind(acc), nil
				}
			}
		}
	}

	tier := highestPriority(candidates)
	sortByID(tier)
	// L1 温热优先：tier 内有 sandbox 温热的账号时只在温组里选（sandbox 探针由
	// api 侧 warm-set 预热子系统注入；未注入时 SandboxWarm 恒 false，等同关闭）。
	if cfg.WarmPreference {
		warm := make([]*pool.Account, 0, len(tier))
		for _, a := range tier {
			if a.SandboxWarm() {
				warm = append(warm, a)
			}
		}
		if len(warm) > 0 {
			tier = warm
		}
	}
	picked := s.pickStrategy(cfg.Strategy, req.Model, tier)
	if picked == nil {
		return nil, unavailableFrom(accounts, req, cfg, now)
	}
	return bind(picked), nil
}

func (s *Scheduler) pickStrategy(strategy Strategy, model string, tier []*pool.Account) *pool.Account {
	switch strategy {
	case StrategyFillFirst:
		if len(tier) == 0 {
			return nil
		}
		return tier[0]
	case StrategyWeighted:
		return s.weight.pick("cursor:"+model, tier)
	default:
		return s.rr.pick("cursor:"+model, tier)
	}
}

func collectReady(accounts []*pool.Account, req Request, cfg Config, now time.Time, limits LimitResolver) []*pool.Account {
	out := make([]*pool.Account, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if req.Tried != nil {
			if _, tried := req.Tried[a.ID]; tried {
				continue
			}
		}
		if req.Group != "" && !inGroup(a.Groups(), req.Group) {
			continue
		}
		overlayRPM, overlayDaily := 0, 0
		if limits != nil {
			overlayRPM, overlayDaily = limits(a.Groups())
		}
		if !a.ReadyWithLimits(req.Model, now, cfg.DisableCooling, overlayRPM, overlayDaily) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func unavailableFrom(accounts []*pool.Account, req Request, cfg Config, now time.Time) error {
	if len(accounts) == 0 {
		return newUnavailable(req.Model, 0, false)
	}
	coolCount := 0
	var earliest time.Time
	for _, a := range accounts {
		if a == nil || !a.Enabled() {
			continue
		}
		next := a.NextRetryAt(req.Model, now)
		if next.IsZero() || !now.Before(next) {
			continue
		}
		coolCount++
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	reset := time.Duration(0)
	if !earliest.IsZero() {
		reset = earliest.Sub(now)
		if reset < 0 {
			reset = 0
		}
	}
	return newUnavailable(req.Model, reset, coolCount > 0)
}

func cacheKey(sessionID, model string) string {
	return "cursor::" + sessionID + "::" + model
}

func findID(accounts []*pool.Account, id string) *pool.Account {
	for _, a := range accounts {
		if a != nil && a.ID == id {
			return a
		}
	}
	return nil
}

func inGroup(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}
