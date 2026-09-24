package emulation

import (
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"prism-2api/internal/secret"
)

const (
	ModeUniform     = "uniform"
	ModeIndependent = "independent"

	ProviderVendor = "vendor"
	ProviderTavily = "tavily"
	ProviderAuto   = "auto"
)

// Config 是协议外观（缓存 / WebSearch / 签名）的持久化配置。
type Config struct {
	Cache     CacheConfig     `json:"cache"`
	WebSearch WebSearchConfig `json:"web_search"`
	Signature SignatureConfig `json:"signature"`
}

// CacheConfig 控制提示词缓存模拟。
type CacheConfig struct {
	Enabled       bool    `json:"enabled"`
	Mode          string  `json:"mode"` // uniform | independent
	Ratio         float64 `json:"ratio"`
	RatioMin      float64 `json:"ratio_min"`
	RatioMax      float64 `json:"ratio_max"`
	CreationRatio float64 `json:"creation_ratio"`
	ReadRatio     float64 `json:"read_ratio"`
	ReadRatioMin  float64 `json:"read_ratio_min"`
	ReadRatioMax  float64 `json:"read_ratio_max"`
	// ForceHit 跳过前缀匹配，所有请求（含首轮）直接按读取区间报 cache_read。
	ForceHit            bool `json:"force_hit"`
	MinTokens           int  `json:"min_tokens"`
	OpusMinTokens       int  `json:"opus_min_tokens"`
	FallbackBreakpoints bool `json:"fallback_breakpoints"`
	TTL5mSeconds        int  `json:"ttl_5m_seconds"`
	TTL1hSeconds        int  `json:"ttl_1h_seconds"`
}

// WebSearchConfig 控制 web_search 拦截与搜索后端。
type WebSearchConfig struct {
	Enabled    bool        `json:"enabled"`
	Provider   string      `json:"provider"` // vendor | tavily | auto
	MaxResults int         `json:"max_results"`
	TavilyKeys []TavilyKey `json:"tavily_keys,omitempty"`
}

// TavilyKey 是 Tavily key 池中的一条。
type TavilyKey struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Key        string `json:"key,omitempty"`
	Enabled    bool   `json:"enabled"`
	LastError  string `json:"last_error,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

// SignatureConfig 控制 thinking signature_delta。
type SignatureConfig struct {
	Enabled bool `json:"enabled"`
}

// DefaultConfig 默认开启缓存 / 签名 / web_search（上游优先，失败走 Tavily）。
func DefaultConfig() Config {
	return Config{
		Cache: CacheConfig{
			Enabled:             true,
			Mode:                ModeUniform,
			Ratio:               1,
			RatioMin:            1,
			RatioMax:            1,
			CreationRatio:       1,
			ReadRatio:           1,
			ReadRatioMin:        1,
			ReadRatioMax:        1,
			MinTokens:           1024,
			OpusMinTokens:       4096,
			FallbackBreakpoints: true,
			TTL5mSeconds:        300,
			TTL1hSeconds:        3600,
		},
		WebSearch: WebSearchConfig{
			Enabled:    true,
			Provider:   ProviderAuto,
			MaxResults: 5,
		},
		Signature: SignatureConfig{Enabled: true},
	}
}

func (c Config) normalize() Config {
	if c.Cache.Mode != ModeIndependent {
		c.Cache.Mode = ModeUniform
	}
	c.Cache.Ratio = clampRatio(c.Cache.Ratio, 1)
	c.Cache.CreationRatio = clampRatio(c.Cache.CreationRatio, 1)
	c.Cache.ReadRatio = clampRatio(c.Cache.ReadRatio, 1)
	// 区间未填（双双为 0）时由旧单值派生，存量配置行为不变。
	c.Cache.RatioMin, c.Cache.RatioMax = normalizeRatioRange(c.Cache.RatioMin, c.Cache.RatioMax, c.Cache.Ratio)
	c.Cache.ReadRatioMin, c.Cache.ReadRatioMax = normalizeRatioRange(c.Cache.ReadRatioMin, c.Cache.ReadRatioMax, c.Cache.ReadRatio)
	if c.Cache.MinTokens <= 0 {
		c.Cache.MinTokens = 1024
	}
	if c.Cache.OpusMinTokens <= 0 {
		c.Cache.OpusMinTokens = 4096
	}
	if c.Cache.TTL5mSeconds <= 0 {
		c.Cache.TTL5mSeconds = 300
	}
	if c.Cache.TTL1hSeconds <= 0 {
		c.Cache.TTL1hSeconds = 3600
	}
	switch strings.ToLower(strings.TrimSpace(c.WebSearch.Provider)) {
	case ProviderVendor, ProviderTavily, ProviderAuto:
		c.WebSearch.Provider = strings.ToLower(strings.TrimSpace(c.WebSearch.Provider))
	default:
		c.WebSearch.Provider = ProviderAuto
	}
	if c.WebSearch.MaxResults <= 0 {
		c.WebSearch.MaxResults = 5
	}
	if c.WebSearch.MaxResults > 20 {
		c.WebSearch.MaxResults = 20
	}
	return c
}

func (c CacheConfig) ttl5m() time.Duration {
	return time.Duration(c.TTL5mSeconds) * time.Second
}

func (c CacheConfig) ttl1h() time.Duration {
	return time.Duration(c.TTL1hSeconds) * time.Second
}

func (c CacheConfig) minTokensFor(model string) int {
	if isOpusModel(model) {
		return c.OpusMinTokens
	}
	return c.MinTokens
}

func (c CacheConfig) uniformRatio() float64 {
	return clampRatio(c.Ratio, 1)
}

// sampleUniformRatio 每请求在 [RatioMin, RatioMax] 均匀采样；区间相等时退化为定值。
func (c CacheConfig) sampleUniformRatio() float64 {
	return sampleRatioRange(c.RatioMin, c.RatioMax)
}

// sampleReadRatio 读取侧采样：independent 用读取区间，uniform 回落统一区间。
func (c CacheConfig) sampleReadRatio() float64 {
	if c.Mode == ModeIndependent {
		return sampleRatioRange(c.ReadRatioMin, c.ReadRatioMax)
	}
	return c.sampleUniformRatio()
}

func sampleRatioRange(lo, hi float64) float64 {
	lo = clampRatio(lo, 1)
	hi = clampRatio(hi, 1)
	if hi <= lo {
		return lo
	}
	return lo + rand.Float64()*(hi-lo)
}

// normalizeRatioRange 归一化倍率区间：clamp 到 [0,1]，min>max 交换，未填时用 fallback 派生。
func normalizeRatioRange(lo, hi, fallback float64) (float64, float64) {
	if lo <= 0 && hi <= 0 {
		lo, hi = fallback, fallback
	}
	lo = clampRatio(lo, 0)
	hi = clampRatio(hi, 0)
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

func (c CacheConfig) creationRatio() float64 {
	if c.Mode == ModeIndependent {
		return clampRatio(c.CreationRatio, 1)
	}
	return c.uniformRatio()
}

func (c CacheConfig) readRatio() float64 {
	if c.Mode == ModeIndependent {
		return clampRatio(c.ReadRatio, 1)
	}
	return c.uniformRatio()
}

// clampRatio 把倍率限制在 [0,1]。NaN 用 fallback（默认 1）。
func clampRatio(ratio, fallback float64) float64 {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return fallback
	}
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// Preview 按「整段前缀都可缓存」估算首轮写入 / 次轮命中，方便管理页对倍率。
func (c CacheConfig) Preview(total int) (first, second Usage) {
	if total < 0 {
		total = 0
	}
	firstCreate := scaleTokens(total, c.creationRatio())
	first = Usage{
		InputTokens:              total - firstCreate,
		CacheCreationInputTokens: firstCreate,
		CacheCreation5m:          firstCreate,
	}
	secondRead := scaleTokens(total, c.readRatio())
	second = Usage{
		InputTokens:          total - secondRead,
		CacheReadInputTokens: secondRead,
	}
	reserveUncachedTail(&first, total)
	reserveUncachedTail(&second, total)
	return first, second
}

func isOpusModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "opus")
}

// PublicView 返回给管理端的配置（Tavily key 只留尾号）。
func (c Config) PublicView() Config {
	out := c
	if len(out.WebSearch.TavilyKeys) == 0 {
		return out
	}
	keys := make([]TavilyKey, len(out.WebSearch.TavilyKeys))
	for i, k := range out.WebSearch.TavilyKeys {
		keys[i] = TavilyKey{
			ID:         k.ID,
			Name:       k.Name,
			Key:        maskSecret(secret.Open(k.Key)),
			Enabled:    k.Enabled,
			LastError:  k.LastError,
			LastUsedAt: k.LastUsedAt,
		}
	}
	out.WebSearch.TavilyKeys = keys
	return out
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

func looksMasked(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.Contains(s, "•") || strings.Contains(s, "*")
}
