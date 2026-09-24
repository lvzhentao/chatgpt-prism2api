package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/auth"
	"prism-2api/internal/egress"
	"prism-2api/internal/failclass"
	"prism-2api/internal/secret"
)

// Account 是池中的一个账号。调度字段与 kiro CredentialEntry / CPA Auth 对齐，
// 令牌仍用本仓库 TokenManager。冷却时长走 failclass（kiro 公式）。
type Account struct {
	Name   string
	ID     string
	Tokens *auth.TokenManager
	Client adapter.Client

	mu sync.Mutex

	enabled       bool
	disableReason string
	priority      int
	weight        int64
	groups        []string
	proxyURL      string
	proxyID       string
	clientType    string
	rpm           int
	dailyMax      int

	failCount      int
	failClass      failclass.Class
	cooldownUntil  time.Time
	cooldownReason failclass.Class
	triggerCount   int
	modelStates    map[string]ModelState
	lastUsedAt     time.Time
	successCount   int64

	// 限流：过去 60s 滑动窗口（进程内）+ UTC 日历日计数（落盘）。
	useTimes   []time.Time
	dailyCount int
	dailyDay   string

	nowFn   func() time.Time
	limitFn func(groups []string) (rpm, dailyMax int)

	// 单号并发门控：inflight 当前在飞数；maxConcurrent 上限（<=0 = 不限制）。
	// 本上游没有速率限制，默认不设上限；管理端给了 account_concurrency 才启用。
	// 429（RateLimit 类）触发 degrade → 上限立即降为 degraded 值（degraded<=0 = 不降级），
	// 不自动恢复，管理端可见并可手动改回（改配置重启 / ClearCooldown 不影响）。
	inflight      int
	maxConcurrent int
	degraded      bool
	concurrencyFn func() (base, degraded int)

	vendorUsage    adapter.UsageSnapshot
	usageUpdatedAt time.Time
	usageLastError string

	persist func()
}

// ModelState 是单模型冷却（CPA model state / 计划表 Unavailable）。
type ModelState struct {
	CooldownUntil time.Time       `json:"cooldown_until"`
	Reason        failclass.Class `json:"reason,omitempty"`
	TriggerCount  int             `json:"trigger_count,omitempty"`
}

func newAccount(name string) *Account {
	return &Account{
		Name:          name,
		ID:            newUUID(),
		enabled:       true,
		weight:        1,
		maxConcurrent: 0, // 0 = 不限制
	}
}

func (a *Account) copyMetaFrom(src *Account) {
	if src == nil {
		return
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	a.ID = src.ID
	a.enabled = src.enabled
	a.disableReason = src.disableReason
	a.priority = src.priority
	a.weight = src.weight
	a.groups = append([]string(nil), src.groups...)
	a.proxyURL = src.proxyURL
	a.proxyID = src.proxyID
	a.clientType = src.clientType
	a.rpm = src.rpm
	a.dailyMax = src.dailyMax
}

func (a *Account) setPersist(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.persist = fn
}

func (a *Account) save() {
	a.mu.Lock()
	fn := a.persist
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (a *Account) Priority() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.priority
}

func (a *Account) SetPriority(p int) {
	a.mu.Lock()
	a.priority = p
	a.mu.Unlock()
	a.save()
}

func (a *Account) Weight() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.weight <= 0 {
		return 1
	}
	return a.weight
}

func (a *Account) SetWeight(w int64) {
	a.mu.Lock()
	if w <= 0 {
		w = 1
	}
	a.weight = w
	a.mu.Unlock()
	a.save()
}

func (a *Account) Groups() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.groups...)
}

// SetGroups 写入组成员（trim + 去重保序）。不查 groups 注册表；校验走 groups.Store.SetGroups。
func (a *Account) SetGroups(names []string) {
	a.mu.Lock()
	a.groups = cleanGroupNames(names)
	a.mu.Unlock()
	a.save()
}

// RenameGroup 把 groups[] 里的 old 换成 new（已有 new 则只删 old）。
func (a *Account) RenameGroup(oldName, newName string) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return
	}
	a.mu.Lock()
	changed := false
	hasNew := false
	for _, g := range a.groups {
		if g == newName {
			hasNew = true
			break
		}
	}
	next := make([]string, 0, len(a.groups))
	for _, g := range a.groups {
		if g == oldName {
			changed = true
			if !hasNew {
				next = append(next, newName)
				hasNew = true
			}
			continue
		}
		next = append(next, g)
	}
	if changed {
		a.groups = next
	}
	a.mu.Unlock()
	if changed {
		a.save()
	}
}

func (a *Account) RPM() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rpm
}

func (a *Account) SetRPM(n int) {
	if n < 0 {
		n = 0
	}
	a.mu.Lock()
	a.rpm = n
	a.mu.Unlock()
	a.save()
}

func (a *Account) DailyMax() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyMax
}

func (a *Account) SetDailyMax(n int) {
	if n < 0 {
		n = 0
	}
	a.mu.Lock()
	a.dailyMax = n
	a.mu.Unlock()
	a.save()
}

// SetLimitResolver 注入组 overlay（账号 rpm/daily_max 为 0 时填空）。调度器也可另设一份。
func (a *Account) SetLimitResolver(fn func(groups []string) (rpm, dailyMax int)) {
	a.mu.Lock()
	a.limitFn = fn
	a.mu.Unlock()
}

// SetConcurrencyResolver 注入并发上限解析（来自 RuntimeConfig，热改即时生效）。
// base 为正常并发上限，degraded 为 429 降级后的上限；<=0 分别表示不限制 / 不降级。
func (a *Account) SetConcurrencyResolver(fn func() (base, degraded int)) {
	a.mu.Lock()
	a.concurrencyFn = fn
	if !a.degraded && fn != nil {
		// 未降级号跟随最新 base（0 也是合法值 = 不限制）；已降级号保持降级。
		base, _ := fn()
		a.maxConcurrent = base
	}
	a.mu.Unlock()
}

// concurrencyLimitLocked 当前并发上限（须持锁）：降级后取 degraded，否则取 base。
// <=0 表示不限制——上游没有速率限制，默认就是这么用的。
func (a *Account) concurrencyLimitLocked() int {
	base, degraded := a.maxConcurrent, 0
	if a.concurrencyFn != nil {
		base, degraded = a.concurrencyFn()
	}
	if a.degraded {
		return degraded
	}
	return base
}

// fullLocked 是否已满（须持锁）。上限 <=0 时永远不满。
func (a *Account) fullLocked() bool {
	limit := a.concurrencyLimitLocked()
	return limit > 0 && a.inflight >= limit
}

// Acquire 占用一个并发槽位；满号返回 false（调用方换号）。不设上限时永远成功。
func (a *Account) Acquire() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fullLocked() {
		return false
	}
	a.inflight++
	return true
}

// Release 释放一个并发槽位。
func (a *Account) Release() {
	a.mu.Lock()
	if a.inflight > 0 {
		a.inflight--
	}
	a.mu.Unlock()
}

// Inflight 当前在飞请求数（快照用途）。
func (a *Account) Inflight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inflight
}

// DegradeConcurrency 429 触发：并发上限立即降至 degraded，单向；degraded<=0 时等于不降级。
func (a *Account) DegradeConcurrency() {
	a.mu.Lock()
	a.degraded = true
	a.mu.Unlock()
}

// ConcurrencySnapshot 并发状态（管理端展示）。
func (a *Account) ConcurrencySnapshot() (inflight, limit int, degraded bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inflight, a.concurrencyLimitLocked(), a.degraded
}

func (a *Account) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enabled
}

func (a *Account) CooldownUntil() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cooldownUntil
}

func (a *Account) ModelCooldownUntil(model string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model == "" || a.modelStates == nil {
		return time.Time{}
	}
	return a.modelStates[model].CooldownUntil
}

// available 判断账号当前是否可用（已启用、未冷却、令牌可取/可自动刷新）。
func (a *Account) available() bool {
	return a.Ready("", time.Now(), false)
}

// Ready 对应 CPA isAuthBlockedForModel 的反面：启用、有令牌、全局/模型冷却未挡住，且未超 RPM/日限。
func (a *Account) Ready(model string, now time.Time, disableCooling bool) bool {
	return a.ReadyWithLimits(model, now, disableCooling, 0, 0)
}

// ReadyWithLimits 在 Ready 之上叠加组 overlay：账号 rpm/daily_max==0 时用 overlay。
func (a *Account) ReadyWithLimits(model string, now time.Time, disableCooling bool, overlayRPM, overlayDaily int) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	enabled := a.enabled
	until := a.cooldownUntil
	var modelUntil time.Time
	if model != "" && a.modelStates != nil {
		modelUntil = a.modelStates[model].CooldownUntil
	}
	limited := a.overLimitLocked(now, overlayRPM, overlayDaily)
	full := a.fullLocked()
	a.mu.Unlock()
	if !enabled || limited || full {
		return false
	}
	if !disableCooling {
		if !until.IsZero() && now.Before(until) {
			return false
		}
		if !modelUntil.IsZero() && now.Before(modelUntil) {
			return false
		}
	}
	if a.Tokens == nil {
		return false
	}
	// 只做本地判断：token 无效直接跳过该号，刷新交给保活循环。
	// 这里绝不能走 Token() 的同步刷新（relogin 是 150s 级浏览器流程，
	// 选号器串行扫全池时会被连环卡死）。
	if a.Tokens.TokenLocal() == nil {
		return false
	}
	return true
}

// NextRetryAt 返回全局与该模型冷却中更早的恢复时间（供 429 Retry-After）。
func (a *Account) NextRetryAt(model string, now time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	var next time.Time
	if !a.cooldownUntil.IsZero() && now.Before(a.cooldownUntil) {
		next = a.cooldownUntil
	}
	if model != "" && a.modelStates != nil {
		until := a.modelStates[model].CooldownUntil
		if !until.IsZero() && now.Before(until) && (next.IsZero() || until.Before(next)) {
			next = until
		}
	}
	return next
}

// skipUseCount 的失败不计入 RPM/日限（请求过错 / 取消 / 地区 / 空分类）。
func skipUseCount(c failclass.Class) bool {
	return c == failclass.None || c == failclass.Canceled ||
		c == failclass.Region || c == failclass.BadRequest
}

// MarkFailure 按 failclass.Result 记录失败。请求过错 / 取消 / 地区不冷却、不计数。
// 其余类（含 Cool=false 的连接生命周期 Server）计入 RPM/日限。
// 可自愈类用 kiro 递增冷却；不可自愈类用 24h。Unavailable 且带 model 时只冷却该模型。
func (a *Account) MarkFailure(res failclass.Result, model string) {
	if skipUseCount(res.Class) {
		return
	}
	a.mu.Lock()
	now := a.now()
	if !skipUseCount(res.Class) {
		a.lastUsedAt = now
		a.recordUseLocked(now)
	}
	if res.Class == failclass.Quota && (a.enabled || isQuotaDisableReason(a.disableReason)) {
		a.enabled = false
		if a.disableReason == "" {
			a.disableReason = "quota"
		}
	}
	// 429（RateLimit）：并发上限立即降级（account_concurrency_429，0 = 不降级），冷却逻辑保持不变。
	if res.Class == failclass.RateLimit {
		a.degraded = true
	}
	// 401/403（Auth）= 上游吊销 session：作废本地 access token 让号退出选号
	// （冷却已停用，若不摘 token，吊销号会一直"就绪"被反复撞 403——线上实测
	// 单号 10 分钟被撞 60+ 次）。凭据束保留，保活循环走代理重登成功后自动回池。
	// 但 403 ≠ 必然吊销：上游基础设施故障也回 403（2026-09-20 21:49 实锤：
	// create project RBAC access denied / backend 403 波，30 分钟 806 次 auth
	// 失败把全池 token 处决干净，PG 从 216 有 token 掉到 45）。401 无歧义
	// （token 过期/无效）立即作废；403 要求连续第 2 次才作废——故障波里每号
	// 最多白挨 2 次即退出，真吊销号也在第 2 撞退出，不再一波团灭。
	if res.Class == failclass.Auth && a.Tokens != nil {
		if res.HTTPStatus != http.StatusForbidden || a.failClass == failclass.Auth {
			a.Tokens.DropAccessToken()
		}
	}
	if !res.Cool || skipUseCount(res.Class) {
		if !skipUseCount(res.Class) && a.overLimitLocked(now, 0, 0) {
			a.applyClassCooldownLocked(failclass.RateLimit, "", now)
		}
		a.mu.Unlock()
		a.save()
		return
	}
	a.applyClassCooldownLocked(res.Class, model, now)
	a.mu.Unlock()
	a.save()
}

// ClearCooldown 管理端手动解除冷却，不增加成功计数。
func (a *Account) ClearCooldown() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.failCount = 0
	a.failClass = failclass.None
	a.cooldownUntil = time.Time{}
	a.cooldownReason = ""
	a.triggerCount = 0
	a.mu.Unlock()
	a.save()
}

// MarkSuccess 对应 kiro clear_cooldown：清冷却与失败计数，并记一次成功请求。
// 若本次计数触顶，再打 RateLimit 冷却，cooldown_reason 可见。
func (a *Account) MarkSuccess() {
	a.mu.Lock()
	now := a.now()
	a.failCount = 0
	a.failClass = failclass.None
	a.cooldownUntil = time.Time{}
	a.cooldownReason = ""
	a.triggerCount = 0
	a.successCount++
	a.lastUsedAt = now
	a.recordUseLocked(now)
	if a.overLimitLocked(now, 0, 0) {
		a.applyClassCooldownLocked(failclass.RateLimit, "", now)
	}
	a.mu.Unlock()
	a.save()
}

// RecordUse 把一次请求计入滑动 60s 窗口与 UTC 日计数。触顶则 RateLimit 冷却。
func (a *Account) RecordUse() {
	a.mu.Lock()
	now := a.now()
	a.lastUsedAt = now
	a.recordUseLocked(now)
	if a.overLimitLocked(now, 0, 0) {
		a.applyClassCooldownLocked(failclass.RateLimit, "", now)
	}
	a.mu.Unlock()
	a.save()
}

// TouchUse 与 RecordUse 相同（测试 / 未走 MarkSuccess 的计数入口）。
func (a *Account) TouchUse() { a.RecordUse() }

// BeginUse 与 RecordUse 相同（一次真实上游尝试开始）。
func (a *Account) BeginUse() { a.RecordUse() }

func (a *Account) now() time.Time {
	if a.nowFn != nil {
		return a.nowFn()
	}
	return time.Now()
}

func (a *Account) recordUseLocked(now time.Time) {
	a.pruneUsesLocked(now)
	a.useTimes = append(a.useTimes, now)
	a.dailyCount++
}

func (a *Account) pruneUsesLocked(now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(a.useTimes) && !a.useTimes[i].After(cutoff) {
		i++
	}
	if i > 0 {
		a.useTimes = append([]time.Time(nil), a.useTimes[i:]...)
	}
	day := utcDay(now)
	if a.dailyDay != day {
		a.dailyDay = day
		a.dailyCount = 0
	}
}

func (a *Account) overLimitLocked(now time.Time, overlayRPM, overlayDaily int) bool {
	a.pruneUsesLocked(now)
	rpm, daily := a.effectiveLimitsLocked(overlayRPM, overlayDaily)
	if rpm > 0 && len(a.useTimes) >= rpm {
		return true
	}
	if daily > 0 && a.dailyCount >= daily {
		return true
	}
	return false
}

func (a *Account) effectiveLimitsLocked(overlayRPM, overlayDaily int) (rpm, daily int) {
	rpm, daily = a.rpm, a.dailyMax
	if rpm > 0 && daily > 0 {
		return rpm, daily
	}
	gRPM, gDaily := overlayRPM, overlayDaily
	if a.limitFn != nil && (gRPM <= 0 || gDaily <= 0) {
		fr, fd := a.limitFn(append([]string(nil), a.groups...))
		if gRPM <= 0 {
			gRPM = fr
		}
		if gDaily <= 0 {
			gDaily = fd
		}
	}
	if rpm <= 0 {
		rpm = gRPM
	}
	if daily <= 0 {
		daily = gDaily
	}
	return rpm, daily
}

func (a *Account) applyClassCooldownLocked(class failclass.Class, model string, now time.Time) {
	// 冷却机制已停用（业务要求账号不做冷却、始终可用）：失败仍计数与分类，
	// 但不再写入 cooldownUntil / 模型级冷却，调度器不会因失败或限频跳过账号。
	a.failCount++
	a.failClass = class
}

func utcDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
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

// Snapshot 是账号状态快照（供 /v1/accounts 与管理 JSON 返回）。
type Snapshot struct {
	Name                string   `json:"name"`
	ID                  string   `json:"id,omitempty"`
	LoggedIn            bool     `json:"logged_in"`
	ExpiresAt           int64    `json:"expires_at"`
	FailCount           int      `json:"fail_count"`
	Disabled            bool     `json:"disabled"`
	Enabled             bool     `json:"enabled"`
	HasAPIKey           bool     `json:"has_api_key"`
	Priority            int      `json:"priority"`
	Weight              int64    `json:"weight"`
	Groups              []string `json:"groups,omitempty"`
	FailClass           string   `json:"fail_class,omitempty"`
	CooldownUntil       int64    `json:"cooldown_until,omitempty"`
	CooldownReason      string   `json:"cooldown_reason,omitempty"`
	TriggerCount        int      `json:"trigger_count,omitempty"`
	SuccessCount        int64    `json:"success_count,omitempty"`
	ProxyURL            string   `json:"proxy_url,omitempty"`
	ProxyID             string   `json:"proxy_id,omitempty"`
	RPM                 int      `json:"rpm,omitempty"`
	DailyMax            int      `json:"daily_max,omitempty"`
	DisableReason       string   `json:"disable_reason,omitempty"`
	ClientType          string   `json:"client_type,omitempty"`
	Email               string   `json:"email,omitempty"`
	PlanBadge           string   `json:"plan_badge,omitempty"`
	PlanLabel           string   `json:"plan_label,omitempty"`
	MembershipType      string   `json:"membership_type,omitempty"`
	WorkOSID            string   `json:"workos_id,omitempty"`
	UsagePercent        *float64 `json:"usage_percent,omitempty"`
	AutoPercentUsed     *float64 `json:"auto_percent_used,omitempty"`
	APIPercentUsed      *float64 `json:"api_percent_used,omitempty"`
	PlanUsedCents       *float64 `json:"plan_used_cents,omitempty"`
	PlanLimitCents      *float64 `json:"plan_limit_cents,omitempty"`
	OnDemandUsedCents   *float64 `json:"on_demand_used_cents,omitempty"`
	OnDemandLimitCents  *float64 `json:"on_demand_limit_cents,omitempty"`
	UsageError          string   `json:"usage_error,omitempty"`
	UsageUpdatedAt      int64    `json:"usage_updated_at,omitempty"`
	BillingCycleEnd     int64    `json:"billing_cycle_end,omitempty"`
	Unlimited           bool     `json:"unlimited,omitempty"`
	Inflight            int      `json:"inflight,omitempty"`
	ConcurrencyLimit    int      `json:"concurrency_limit,omitempty"`
	ConcurrencyDegraded bool     `json:"concurrency_degraded,omitempty"`
	SandboxWarm         bool     `json:"sandbox_warm,omitempty"`
	SandboxAgeSec       int64    `json:"sandbox_age_sec,omitempty"`
}

// UsageView 是账号官方配额缓存（cockpit usage-summary）。
type UsageView struct {
	SuccessCount int64                 `json:"success_count"`
	Vendor       adapter.UsageSnapshot `json:"vendor"`
	Error        string                `json:"error,omitempty"`
	UpdatedAt    int64                 `json:"updated_at,omitempty"`
}

// Snapshot 返回账号当前状态。
func (a *Account) Snapshot() Snapshot {
	a.mu.Lock()
	fail := a.failCount
	enabled := a.enabled
	until := a.cooldownUntil
	cool := time.Now().Before(until)
	cls := string(a.failClass)
	reason := string(a.cooldownReason)
	trig := a.triggerCount
	prio := a.priority
	weight := a.weight
	groups := append([]string(nil), a.groups...)
	id := a.ID
	success := a.successCount
	proxy := a.proxyURL
	proxyID := a.proxyID
	rpm := a.rpm
	daily := a.dailyMax
	disableReason := a.disableReason
	clientType := a.clientType
	u := a.vendorUsage
	email := u.Email
	planBadge := u.PlanBadge
	planLabel := u.PlanLabel
	membership := u.MembershipType
	workos := u.WorkOSID
	usagePct := u.TotalPercentUsed
	autoPct := u.AutoPercentUsed
	apiPct := u.APIPercentUsed
	planUsed := u.PlanUsedCents
	planLimit := u.PlanLimitCents
	odUsed := u.OnDemandUsedCents
	odLimit := u.OnDemandLimitCents
	usageErr := a.usageLastError
	var usageAt, cycleEnd int64
	if !a.usageUpdatedAt.IsZero() {
		usageAt = a.usageUpdatedAt.Unix()
	}
	cycleEnd = u.BillingCycleEnd
	unlimited := u.Unlimited
	var coolUnix int64
	if cool {
		coolUnix = until.Unix()
	}
	inflight, concLimit, concDegraded := a.inflight, a.concurrencyLimitLocked(), a.degraded
	a.mu.Unlock()
	// 沙箱观测经包级探针注册表读（见 prewarm_probe.go）：sandbox 状态活在 prism
	// 包 httpClient 内，pool 不得 import adapter/prism；锁外读防探针回调重入死锁。
	sbWarm, sbAge := a.sandboxProbeSnapshot()
	// 观测无副作用：Snapshot 被 dashboard 全池遍历，走 Token() 会把每个
	// 失效号都拖进同步 relogin（dashboard 曾因此挂 18 分钟）。只读当前状态。
	tok := a.Tokens.TokenLocal()
	loggedIn := tok != nil && tok.AccessToken != ""
	var exp int64
	if loggedIn {
		exp = auth.JWTExpiry(tok.AccessToken)
	}
	return Snapshot{
		Name: a.Name, ID: id, LoggedIn: loggedIn, ExpiresAt: exp,
		FailCount: fail, Disabled: !enabled || cool, Enabled: enabled,
		HasAPIKey: a.Tokens.HasAPIKey(), Priority: prio, Weight: weight,
		Groups: groups, FailClass: cls, CooldownUntil: coolUnix,
		CooldownReason: reason, TriggerCount: trig, SuccessCount: success,
		ProxyURL: proxy, ProxyID: proxyID, RPM: rpm, DailyMax: daily,
		DisableReason: disableReason, ClientType: clientType,
		Email: email, PlanBadge: planBadge, PlanLabel: planLabel, MembershipType: membership,
		WorkOSID: workos, UsagePercent: usagePct, AutoPercentUsed: autoPct, APIPercentUsed: apiPct,
		PlanUsedCents: planUsed, PlanLimitCents: planLimit,
		OnDemandUsedCents: odUsed, OnDemandLimitCents: odLimit,
		UsageError: usageErr, UsageUpdatedAt: usageAt,
		BillingCycleEnd: cycleEnd, Unlimited: unlimited,
		Inflight: inflight, ConcurrencyLimit: concLimit, ConcurrencyDegraded: concDegraded,
		SandboxWarm: sbWarm, SandboxAgeSec: sbAge,
	}
}

// UsageView 返回本地成功次数 + 缓存的官方配额。
func (a *Account) UsageView() UsageView {
	if a == nil {
		return UsageView{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	v := UsageView{SuccessCount: a.successCount, Vendor: a.vendorUsage, Error: a.usageLastError}
	if !a.usageUpdatedAt.IsZero() {
		v.UpdatedAt = a.usageUpdatedAt.Unix()
	}
	return v
}

// RememberUsage 写入官方配额缓存并落盘。
func (a *Account) RememberUsage(snap adapter.UsageSnapshot, errMsg string) {
	if a == nil {
		return
	}
	name := a.Name
	a.mu.Lock()
	wasEnabled := a.enabled
	a.vendorUsage = snap
	a.usageLastError = errMsg
	a.usageUpdatedAt = time.Now()
	if snap.FetchedAt > 0 {
		a.usageUpdatedAt = time.Unix(snap.FetchedAt, 0)
	}
	if snap.HasQuotaMeters() || snap.Unlimited {
		a.applyQuotaPolicyLocked()
	}
	recovered := !wasEnabled && a.enabled
	a.mu.Unlock()
	a.save()
	if recovered {
		notifyAccountEnabled(name)
	}
}

// accountEnabledHook 是账号「从非 Enabled 变为 Enabled」时的通知钩子。
// pool 包不得依赖 api 包，预热调度器在 api 侧：api 侧 startPrewarm 时调用
// OnAccountEnabled 注册回调，回调内自行防抖（per-account 冷却 + 去重在飞）。
// 未注册时为 nil，notifyAccountEnabled nil 安全；loadAll 加载路径不触发
// （由 boot stagger 覆盖），只在 ApplyAdminPatch / RememberUsage 配额恢复
// （登录完成经 ApplyAdminPatch 间接触发）中确认翻转时调用。
var (
	accountEnabledMu   sync.RWMutex
	accountEnabledHook func(name string)
)

// OnAccountEnabled 注册账号变为 Enabled 时的回调；fn 为 nil 表示清空。
func OnAccountEnabled(fn func(name string)) {
	accountEnabledMu.Lock()
	accountEnabledHook = fn
	accountEnabledMu.Unlock()
}

// notifyAccountEnabled 在确认翻转时调用钩子；调用方须在锁外调用，nil 安全。
func notifyAccountEnabled(name string) {
	if name == "" {
		return
	}
	accountEnabledMu.RLock()
	fn := accountEnabledHook
	accountEnabledMu.RUnlock()
	if fn == nil {
		return
	}
	fn(name)
}

func isQuotaDisableReason(reason string) bool {
	return strings.HasPrefix(reason, "quota")
}

// applyQuotaPolicyLocked 额度打满自动停用；仅恢复先前因额度停用的号，不动手动停用。
func (a *Account) applyQuotaPolicyLocked() {
	reason := a.vendorUsage.QuotaDisableReason()
	if reason != "" {
		if a.enabled || a.disableReason == "" || isQuotaDisableReason(a.disableReason) {
			a.enabled = false
			a.disableReason = reason
		}
		return
	}
	if !a.enabled && isQuotaDisableReason(a.disableReason) {
		a.enabled = true
		a.disableReason = ""
	}
}

// IdentityEmail 返回缓存的 Cursor 邮箱（小写）。
func (a *Account) IdentityEmail() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.ToLower(strings.TrimSpace(a.vendorUsage.Email))
}

// IdentityWorkOS 返回 WorkOS user_…：用量缓存优先，否则从 access JWT 解析。
func (a *Account) IdentityWorkOS() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	id := strings.TrimSpace(a.vendorUsage.WorkOSID)
	a.mu.Unlock()
	if id != "" {
		return id
	}
	if a.Tokens == nil {
		return ""
	}
	tok := a.Tokens.Current()
	if tok == nil {
		return ""
	}
	return auth.IdentitySubject(tok.AccessToken)
}

// CurrentAccessToken 返回当前 access JWT，不触发刷新。
func (a *Account) CurrentAccessToken() string {
	if a == nil || a.Tokens == nil {
		return ""
	}
	tok := a.Tokens.Current()
	if tok == nil {
		return ""
	}
	return tok.AccessToken
}

// RefreshUsage 对照 cockpit refresh_account：打官方用量接口并缓存。
func (a *Account) RefreshUsage(websiteURL string) UsageView {
	if a == nil || a.Client == nil {
		return UsageView{Error: "no client"}
	}
	snap, err := a.Client.FetchUsage(context.Background(), websiteURL)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	a.RememberUsage(snap, errMsg)
	return a.UsageView()
}

// AdminPatch 是管理端 PATCH 账号元数据（指针表示缺省不改）。
type AdminPatch struct {
	Priority *int
	Weight   *int64
	Enabled  *bool
	ProxyURL *string
	ProxyID  *string
	RPM      *int
	DailyMax *int
	Groups   *[]string
}

// ApplyAdminPatch 一次写入调度字段并持久化。不改令牌。
func (a *Account) ApplyAdminPatch(p AdminPatch) {
	if a == nil {
		return
	}
	name := a.Name
	a.mu.Lock()
	wasEnabled := a.enabled
	if p.Priority != nil {
		a.priority = *p.Priority
	}
	if p.Weight != nil {
		w := *p.Weight
		if w <= 0 {
			w = 1
		}
		a.weight = w
	}
	if p.Enabled != nil {
		a.enabled = *p.Enabled
		if *p.Enabled {
			a.disableReason = ""
		}
	}
	if p.ProxyURL != nil {
		a.proxyURL = *p.ProxyURL
	}
	if p.ProxyID != nil {
		a.proxyID = strings.TrimSpace(*p.ProxyID)
	}
	if p.RPM != nil {
		rpm := *p.RPM
		if rpm < 0 {
			rpm = 0
		}
		a.rpm = rpm
	}
	if p.DailyMax != nil {
		daily := *p.DailyMax
		if daily < 0 {
			daily = 0
		}
		a.dailyMax = daily
	}
	if p.Groups != nil {
		a.groups = cleanGroupNames(*p.Groups)
	}
	becameEnabled := !wasEnabled && a.enabled
	a.mu.Unlock()
	a.save()
	if becameEnabled {
		notifyAccountEnabled(name)
	}
}

// persistedAccount 与旧 Token JSON 兼容：原文件只有 accessToken/refreshToken/api_key，
// 多出来的调度字段缺省即可。保存时 Token 字段与元数据写在同一文件。
type persistedAccount struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	APIKey       string `json:"api_key,omitempty"`

	ID            string   `json:"id,omitempty"`
	Enabled       *bool    `json:"enabled,omitempty"`
	DisableReason string   `json:"disable_reason,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	Weight        int64    `json:"weight,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	ProxyURL      string   `json:"proxy_url,omitempty"`
	ProxyID       string   `json:"proxy_id,omitempty"`
	ClientType    string   `json:"client_type,omitempty"`
	RPM           int      `json:"rpm,omitempty"`
	DailyMax      int      `json:"daily_max,omitempty"`

	FailClass      string                `json:"fail_class,omitempty"`
	CooldownUntil  int64                 `json:"cooldown_until,omitempty"`
	CooldownReason string                `json:"cooldown_reason,omitempty"`
	TriggerCount   int                   `json:"trigger_count,omitempty"`
	ModelStates    map[string]ModelState `json:"model_states,omitempty"`
	LastUsedAt     int64                 `json:"last_used_at,omitempty"`
	SuccessCount   int64                 `json:"success_count,omitempty"`

	CursorUsage    *adapter.UsageSnapshot `json:"cursor_usage,omitempty"`
	UsageUpdatedAt int64                  `json:"usage_updated_at,omitempty"`
	UsageLastError string                 `json:"usage_last_error,omitempty"`

	DailyCount int    `json:"daily_count,omitempty"`
	DailyDay   string `json:"daily_day,omitempty"`
}

func (a *Account) marshalPersisted() ([]byte, error) {
	rec := persistedAccount{}
	if tok := a.Tokens.Current(); tok != nil {
		rec.AccessToken = secret.Seal(tok.AccessToken)
		rec.RefreshToken = secret.Seal(tok.RefreshToken)
		rec.APIKey = secret.Seal(tok.APIKey)
		if rec.APIKey == "" || rec.APIKey == secret.Seal("") {
			rec.APIKey = secret.Seal(a.Tokens.APIKey())
		}
	} else {
		rec.APIKey = secret.Seal(a.Tokens.APIKey())
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	rec.ID = a.ID
	if !a.enabled {
		f := false
		rec.Enabled = &f
	}
	rec.DisableReason = a.disableReason
	rec.Priority = a.priority
	if a.weight != 1 {
		rec.Weight = a.weight
	}
	rec.Groups = a.groups
	rec.ProxyURL = a.proxyURL
	rec.ProxyID = a.proxyID
	rec.ClientType = a.clientType
	rec.RPM = a.rpm
	rec.DailyMax = a.dailyMax
	rec.FailClass = string(a.failClass)
	if !a.cooldownUntil.IsZero() {
		rec.CooldownUntil = a.cooldownUntil.Unix()
	}
	rec.CooldownReason = string(a.cooldownReason)
	rec.TriggerCount = a.triggerCount
	rec.ModelStates = a.modelStates
	if !a.lastUsedAt.IsZero() {
		rec.LastUsedAt = a.lastUsedAt.Unix()
	}
	rec.SuccessCount = a.successCount
	if a.vendorUsage.FetchedAt > 0 || a.vendorUsage.MembershipType != "" || a.usageLastError != "" {
		snap := a.vendorUsage
		rec.CursorUsage = &snap
	}
	if !a.usageUpdatedAt.IsZero() {
		rec.UsageUpdatedAt = a.usageUpdatedAt.Unix()
	}
	rec.UsageLastError = a.usageLastError
	rec.DailyCount = a.dailyCount
	rec.DailyDay = a.dailyDay
	return json.Marshal(rec)
}

func applyPersisted(a *Account, rec persistedAccount) {
	a.ID = rec.ID
	if a.ID == "" {
		a.ID = newUUID()
	}
	a.enabled = rec.Enabled == nil || *rec.Enabled
	a.disableReason = rec.DisableReason
	a.priority = rec.Priority
	a.weight = rec.Weight
	if a.weight == 0 {
		a.weight = 1
	}
	a.groups = rec.Groups
	a.proxyURL = rec.ProxyURL
	a.proxyID = rec.ProxyID
	a.clientType = rec.ClientType
	a.rpm = rec.RPM
	a.dailyMax = rec.DailyMax
	a.failClass = failclass.Class(rec.FailClass)
	// 冷却机制已停用：不恢复存量冷却（全局 cooldownUntil / 模型级 / 触发计数），重启即全部可用。
	a.modelStates = nil
	if rec.LastUsedAt > 0 {
		a.lastUsedAt = time.Unix(rec.LastUsedAt, 0)
	}
	a.successCount = rec.SuccessCount
	if rec.CursorUsage != nil {
		a.vendorUsage = *rec.CursorUsage
	}
	if rec.UsageUpdatedAt > 0 {
		a.usageUpdatedAt = time.Unix(rec.UsageUpdatedAt, 0)
	}
	a.usageLastError = rec.UsageLastError
	a.dailyCount = rec.DailyCount
	a.dailyDay = rec.DailyDay
	if a.vendorUsage.HasQuotaMeters() || a.vendorUsage.Unlimited {
		a.applyQuotaPolicyLocked()
	}
}

func tokenFromPersisted(rec persistedAccount) auth.Token {
	return auth.Token{
		AccessToken:  secret.Open(rec.AccessToken),
		RefreshToken: secret.Open(rec.RefreshToken),
		APIKey:       secret.Open(rec.APIKey),
	}
}

// ProxyURL 账号级上游 HTTP 代理。
func (a *Account) ProxyURL() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyURL
}

// ProxyID 绑定的出口池条目。
func (a *Account) ProxyID() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyID
}

// StableIdentity Resin 粘性账号标识：优先持久 ID，否则用名称。
func (a *Account) StableIdentity() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ID != "" {
		return a.ID
	}
	return a.Name
}

// ApplyClientProxy 账号 proxy 优先，否则用全局 HTTP 正代。
func (a *Account) ApplyClientProxy(global string) {
	if a == nil {
		return
	}
	url := a.ProxyURL()
	if url == "" {
		url = global
	}
	a.ApplyEgress(egress.Settings{Kind: egressKindForProxy(url), HTTPProxyURL: url, Account: a.StableIdentity()})
}

// ApplyEgress 把出口应用到 Cursor 客户端与令牌刷新客户端。
func (a *Account) ApplyEgress(s egress.Settings) {
	if a == nil {
		return
	}
	if s.Account == "" {
		s.Account = a.StableIdentity()
	}
	if a.Client != nil {
		_ = a.Client.SetEgress(s)
	}
	if a.Tokens != nil {
		cli, err := egress.HTTPClient(s, 30*time.Second)
		if err == nil {
			a.Tokens.SetHTTPClient(cli)
		}
	}
}

func egressKindForProxy(u string) string {
	if strings.TrimSpace(u) == "" {
		return egress.KindDirect
	}
	return egress.KindHTTP
}
