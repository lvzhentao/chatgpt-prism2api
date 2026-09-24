// Package failclass 把上游失败分成可调度的类型。
//
// 分类规则来自 CLIProxyAPI internal/clienterror（请求过错 vs 凭据过错）
// 与 kiro.rs src/kiro/cooldown.rs（冷却时长 / 是否自愈）。
// Cursor 特有文案（地区、error_bad_model_name 等）沿用本仓库原 isAccountFatalError 黑名单，
// 映射到同一套类型，而不是另写一套阈值。
package failclass

import (
	"math"
	"time"
)

// Class 是一次上游失败的调度类型。
type Class string

const (
	None        Class = ""
	RateLimit   Class = "rate_limit"
	Server      Class = "server"
	Auth        Class = "auth"
	Quota       Class = "quota"
	Canceled    Class = "canceled"
	Region      Class = "region"
	BadRequest  Class = "bad_request"
	Unavailable Class = "unavailable"
)

// kiro CooldownReason::default_duration / CooldownManager 上限。
const (
	rateLimitBase    = 60 * time.Second
	serverBase       = 120 * time.Second
	unavailableBase  = 300 * time.Second
	authBase         = time.Hour
	quotaBase        = 24 * time.Hour
	maxShortCooldown = 5 * time.Minute
	longCooldown     = 24 * time.Hour
)

// StatusClientClosedRequest 与 CPA clienterror 相同：客户端在代理完成前断开。
const StatusClientClosedRequest = 499

// Result 是 Classify 的完整结论：类型 + CPA 的冷却/换号开关。
type Result struct {
	Class      Class
	Cool       bool // 是否惩罚凭据（CPA shouldSkipCredentialCooldown 的反面）
	Switch     bool // 是否换号重试（请求过错 / 地区 / 客户端取消为 false）
	HTTPStatus int
}

// AutoRecoverable 对应 kiro CooldownReason::is_auto_recoverable。
func (c Class) AutoRecoverable() bool {
	switch c {
	case RateLimit, Server, Unavailable:
		return true
	default:
		return false
	}
}

// BaseDuration 对应 kiro CooldownReason::default_duration。
func (c Class) BaseDuration() time.Duration {
	switch c {
	case RateLimit:
		return rateLimitBase
	case Server:
		return serverBase
	case Unavailable:
		return unavailableBase
	case Auth:
		return authBase
	case Quota:
		return quotaBase
	default:
		return 0
	}
}

// Duration 对应 kiro CooldownManager::calculate_cooldown_duration。
// 可自愈：base * 1.5^(triggerCount-1)，封顶 5 分钟。
// 不可自愈：固定 24 小时（kiro long_cooldown_secs）。
func Duration(class Class, triggerCount int) time.Duration {
	if triggerCount < 1 {
		triggerCount = 1
	}
	base := class.BaseDuration()
	if base <= 0 {
		return 0
	}
	if !class.AutoRecoverable() {
		return longCooldown
	}
	secs := uint64(float64(base/time.Second) * math.Pow(1.5, float64(triggerCount-1)))
	if secs > uint64(maxShortCooldown/time.Second) {
		secs = uint64(maxShortCooldown / time.Second)
	}
	return time.Duration(secs) * time.Second
}
