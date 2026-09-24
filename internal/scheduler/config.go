// Package scheduler 把 CLIProxyAPI 的选号语义迁到 Cursor 单上游。
//
// 对照：
//   - sdk/cliproxy/auth/selector.go（RR / 平滑加权 / FillFirst / SessionAffinitySelector）
//   - sdk/cliproxy/auth/scheduler.go pickSingle（绑定优先于更高 priority；tried 过滤）
//   - sdk/cliproxy/session/identity.go ClaudeMetadataSessionID
//   - navos2api extractModelSessionId（user_id 里的 session_ 片段）
//
// 不搬：pickMixed 多供应商、Codex websocket 偏好、消息内容 hash 兜底。
package scheduler

import "time"

// Strategy 对应 CPA routing.strategy。
type Strategy string

const (
	StrategyRoundRobin Strategy = "round-robin"
	StrategyWeighted   Strategy = "weighted-round-robin"
	StrategyFillFirst  Strategy = "fill-first"
)

// Config 对应 CPA config.example.yaml 的 routing / request-retry 段。
// SessionAffinity 默认开：Cursor 场景要钉 prompt cache；CPA 上游默认 false。
type Config struct {
	Strategy            Strategy
	SessionAffinity     bool
	SessionAffinityTTL  time.Duration
	RequestRetry        int
	MaxRetryCredentials int           // 0 = 试完全部可用号
	MaxRetryInterval    time.Duration // 全冷却时最多睡这么久再试
	DisableCooling      bool
	// WarmPreference 温热优先（TTFB L1）：tier 内有 sandbox 温热的账号时只在温组里选，
	// 耗尽/满槽才落冷组。只影响同优先级内的选择顺序，不改过滤语义。
	WarmPreference bool
}

// DefaultConfig 是 CPA 数字默认值 + Cursor 粘性默认开。
func DefaultConfig() Config {
	return Config{
		Strategy:           StrategyRoundRobin,
		SessionAffinity:    true,
		SessionAffinityTTL: time.Hour,
		RequestRetry:       3,
		MaxRetryInterval:   30 * time.Second,
	}
}

func (c Config) normalized() Config {
	out := c
	switch out.Strategy {
	case StrategyWeighted, StrategyFillFirst, StrategyRoundRobin:
	default:
		out.Strategy = StrategyRoundRobin
	}
	if out.SessionAffinityTTL <= 0 {
		out.SessionAffinityTTL = time.Hour
	}
	if out.RequestRetry < 0 {
		out.RequestRetry = 0
	}
	if out.MaxRetryCredentials < 0 {
		out.MaxRetryCredentials = 0
	}
	if out.MaxRetryInterval < 0 {
		out.MaxRetryInterval = 0
	}
	return out
}

// CredentialLimit 是一次请求最多换号次数。0 表示不限制（试完全部可用号）。
func (c Config) CredentialLimit() int {
	if c.MaxRetryCredentials <= 0 {
		return 0
	}
	return c.MaxRetryCredentials
}
