// Package admin 提供 Web 管理端：运行时配置修改与请求日志。
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/persist"

	"gopkg.in/yaml.v3"
)

// RuntimeConfig 运行时可变配置（持久化到 PostgreSQL / 测试内存，管理员可在线修改）。
// 注：listen_addr / mock_mode 修改后需重启生效（其余字段即时生效）。
type RuntimeConfig struct {
	mu         sync.RWMutex
	APIBaseURL string `json:"api_base_url"`      // 上游 API 地址
	WebsiteURL string `json:"website_url"`       // 登录网站地址
	APIKeyAuth string `json:"api_key_auth"`      // 本站 API 鉴权 key（空 = 不鉴权）
	CacheTTL   int    `json:"cache_ttl_seconds"` // 模型列表缓存秒数
	LogMax     int    `json:"log_max_entries"`   // 请求日志保留条数
	ListenAddr string `json:"listen_addr"`       // 监听地址（重启生效）
	MockMode   bool   `json:"mock_mode"`         // 是否使用内置 mock 后端（重启生效）
	// 管理员账号（登录管理后台用）
	AdminUsername      string `json:"admin_username"`                 // 管理员用户名
	AdminPasswordHash  string `json:"admin_password_hash"`            // PBKDF2-HMAC-SHA256 密码哈希
	MustChangePassword bool   `json:"must_change_password,omitempty"` // 默认 admin123：强制改密

	// 调度（字段名对齐 CPA config.example.yaml；JSON 用 snake_case）。
	RoutingStrategy     string `json:"routing_strategy"`      // round-robin | weighted-round-robin | fill-first
	SessionAffinity     *bool  `json:"session_affinity"`      // nil = 默认 true（粘会话，利于假缓存）
	SessionAffinityTTL  string `json:"session_affinity_ttl"`  // 如 "1h"
	RequestRetry        *int   `json:"request_retry"`         // nil = 3；0 = 外层不等待
	MaxRetryCredentials *int   `json:"max_retry_credentials"` // nil/0 = 试完全部可用号
	MaxRetryIntervalSec *int   `json:"max_retry_interval"`    // nil = 30 秒
	DisableCooling      bool   `json:"disable_cooling"`

	AllowRemoteAdmin      bool              `json:"allow_remote_admin"`
	AccountConcurrency    int               `json:"account_concurrency"`     // 单号并发上限；0 = 不限制
	AccountConcurrency429 int               `json:"account_concurrency_429"` // 账号 429 后并发降级值；0 = 不降级
	GatewayConcurrency    int               `json:"gateway_concurrency"`     // M2 入口并发闸门总槽位；0 = 自动（池号数×单号并发）
	GatewayQueueSec       int               `json:"gateway_queue_sec"`       // 闸门排队超时秒（超时 429+Retry-After）；0 = 30
	PrewarmAccounts       int               `json:"prewarm_accounts"`        // warm-set 容量；默认 16，0 = 关闭预热
	PrewarmIntervalSec    int               `json:"prewarm_interval_sec"`    // 巡检间隔秒；默认 60（沙箱 TTL 2 分钟，刷新必须在过期前完成）
	WarmPreference        bool              `json:"warm_preference"`         // 温热优先选号（TTFB L1）；默认关，灰度后开
	ProxyURL              string            `json:"proxy_url,omitempty"`
	ModelRoutes           map[string]string `json:"model_routes,omitempty"`
	HistoryCompress       string            `json:"history_compress,omitempty"`
	SystemPrompt          string            `json:"system_prompt,omitempty"`        // 注入到上游的系统指令；空=默认（见 PromptText）
	SystemPromptMode      string            `json:"system_prompt_mode,omitempty"`   // inject（默认）| off
	IdentityGuardEnabled  *bool             `json:"identity_guard_enabled"`         // nil=默认开：身份话题才清洗，普通问答不动
	IdentityAnswerText    string            `json:"identity_answer_text,omitempty"` // 身份闸回退答句；空=默认答句
	promptSeededFromEnv   bool              // env 已播种过一次（老库升级用，不持久化）
	LogRetentionDays      int               `json:"log_retention_days,omitempty"`
	BatchConcurrency      int               `json:"batch_concurrency,omitempty"`
	ResinEnabled          bool              `json:"resin_enabled,omitempty"`
	ResinURL              string            `json:"resin_url,omitempty"`
	ResinPlatform         string            `json:"resin_platform,omitempty"`

	backend persist.Backend
	path    string // 仅展示 / 一次性迁入路径，不再作为写入目标
}

// Default 基于启动配置创建运行时配置。
func Default(baseURL, websiteURL, apiKeyAuth, listenAddr string, mock bool) *RuntimeConfig {
	aff := true
	retry := 3
	maxCred := 0
	maxWait := 30
	c := &RuntimeConfig{
		APIBaseURL:            baseURL,
		WebsiteURL:            websiteURL,
		APIKeyAuth:            apiKeyAuth,
		CacheTTL:              600,
		LogMax:                5000,
		ListenAddr:            listenAddr,
		MockMode:              mock,
		RoutingStrategy:       "round-robin",
		SessionAffinity:       &aff,
		SessionAffinityTTL:    "1h",
		RequestRetry:          &retry,
		MaxRetryCredentials:   &maxCred,
		MaxRetryIntervalSec:   &maxWait,
		HistoryCompress:       "code",
		AccountConcurrency:    0,
		AccountConcurrency429: 0,
		PrewarmAccounts:       16,
		PrewarmIntervalSec:    60,
		ResinPlatform:         "Default",
	}
	applyPromptDefaultsFromEnv(c)
	applyAllowRemoteFromEnv(c)
	return c
}

// applyPromptDefaultsFromEnv 用进程环境给注入文案播种（只给空字段填值，
// 已持久化的文案优先；PRISM_SYSTEM_PROMPT=off/none/- 表示关掉注入）。
func applyPromptDefaultsFromEnv(c *RuntimeConfig) {
	if c == nil {
		return
	}
	if c.SystemPrompt == "" {
		if f := strings.TrimSpace(os.Getenv("PRISM_SYSTEM_PROMPT_FILE")); f != "" {
			if b, err := os.ReadFile(f); err == nil {
				if text := strings.TrimSpace(string(b)); text != "" {
					c.SystemPrompt = text
				}
			}
		}
	}
	if c.SystemPrompt == "" {
		if v, ok := os.LookupEnv("PRISM_SYSTEM_PROMPT"); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "off", "none", "-":
				c.SystemPromptMode = "off"
			default:
				if strings.TrimSpace(v) != "" {
					c.SystemPrompt = strings.ReplaceAll(v, `\n`, "\n")
				}
			}
		}
	}
}

// Open 从 Backend 加载运行时配置；库空则写入默认管理员并强制改密。
func Open(b persist.Backend, baseURL, websiteURL, apiKeyAuth, listenAddr string, mock bool) (*RuntimeConfig, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	defaults := Default(baseURL, websiteURL, apiKeyAuth, listenAddr, mock)
	data, err := b.LoadDoc(persist.KindConfig, persist.IDMain)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		defaults.backend = b
		defaults.AdminUsername = DefaultAdminUsername
		defaults.applyDefaultPassword()
		if err := defaults.Save(); err != nil {
			return nil, err
		}
		return defaults, nil
	}
	var loaded RuntimeConfig
	if err := unmarshalConfig(data, &loaded); err != nil {
		return nil, err
	}
	loaded.backend = b
	if loaded.APIBaseURL == "" {
		loaded.APIBaseURL = defaults.APIBaseURL
	}
	if loaded.WebsiteURL == "" {
		loaded.WebsiteURL = defaults.WebsiteURL
	}
	if loaded.ListenAddr == "" {
		loaded.ListenAddr = defaults.ListenAddr
	}
	if loaded.CacheTTL <= 0 {
		loaded.CacheTTL = defaults.CacheTTL
	}
	if loaded.LogMax <= 0 {
		loaded.LogMax = defaults.LogMax
	}
	if loaded.LogRetentionDays <= 0 {
		loaded.LogRetentionDays = 7
	}
	if loaded.HistoryCompress == "" {
		loaded.HistoryCompress = "code"
	}
	if loaded.BatchConcurrency <= 0 {
		loaded.BatchConcurrency = 4
	}
	// account_concurrency / account_concurrency_429 不设回退：<=0 是"不限制/不降级"的合法值。
	// prewarm_* 用"是否出现在文档里"区分"未配置（回退默认）"与"显式 0（关闭/回退 90）"：
	// 老库缺少字段时回填默认，新值经 Update 校验后持久化；正负零值 JSON 不可区分但语义一致。
	if !prewarmKeyPresent(data, "prewarm_accounts") {
		loaded.PrewarmAccounts = 16
	}
	if !prewarmKeyPresent(data, "prewarm_interval_sec") {
		loaded.PrewarmIntervalSec = 60
	}
	if loaded.ResinPlatform == "" {
		loaded.ResinPlatform = "Default"
	}
	if loaded.AdminUsername == "" {
		loaded.AdminUsername = DefaultAdminUsername
	}
	dirty := false
	if loaded.AdminPasswordHash == "" {
		loaded.applyDefaultPassword()
		dirty = true
	} else if loaded.isDefaultPassword() && !loaded.MustChangePassword {
		loaded.MustChangePassword = true
		dirty = true
	}
	if !loaded.promptSeededFromEnv {
		applyPromptDefaultsFromEnv(&loaded)
		loaded.promptSeededFromEnv = true
		dirty = true
	}
	if applyAllowRemoteFromEnv(&loaded) {
		dirty = true
	}
	if dirty {
		if err := loaded.Save(); err != nil {
			return nil, err
		}
	}
	applyRoutingDefaults(&loaded)
	return &loaded, nil
}

// LoadOrCreate 一次性读入旧 JSON/YAML 到内存 Backend（测试 / 迁入）。不回写文件。
func LoadOrCreate(path, baseURL, websiteURL, apiKeyAuth, listenAddr string, mock bool) (*RuntimeConfig, error) {
	mem := persist.NewMemory()
	if err := persist.ImportConfigFile(mem, path); err != nil {
		return nil, err
	}
	rc, err := Open(mem, baseURL, websiteURL, apiKeyAuth, listenAddr, mock)
	if rc != nil {
		rc.path = path
	}
	return rc, err
}

func applyRoutingDefaults(rc *RuntimeConfig) {
	if rc.RoutingStrategy == "" {
		rc.RoutingStrategy = "round-robin"
	}
	if rc.SessionAffinity == nil {
		v := true
		rc.SessionAffinity = &v
	}
	if rc.SessionAffinityTTL == "" {
		rc.SessionAffinityTTL = "1h"
	}
	if rc.RequestRetry == nil {
		v := 3
		rc.RequestRetry = &v
	}
	if rc.MaxRetryCredentials == nil {
		v := 0
		rc.MaxRetryCredentials = &v
	}
	if rc.MaxRetryIntervalSec == nil {
		v := 30
		rc.MaxRetryIntervalSec = &v
	}
}

// Save 把配置写入 Backend。
func (rc *RuntimeConfig) Save() error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.saveLocked()
}

// saveLocked 写入 Backend（调用方须持锁；避免 RWMutex 写锁内重入 RLock 死锁）。
func (rc *RuntimeConfig) saveLocked() error {
	if rc.backend == nil {
		return nil
	}
	data, err := json.Marshal(rc)
	if err != nil {
		return err
	}
	return rc.backend.SaveDoc(persist.KindConfig, persist.IDMain, data)
}

// SetPath 设置配置文件路径（用于页面展示）。
func (rc *RuntimeConfig) SetPath(path string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.path = path
}

// Get 返回当前配置（去掉内部字段的拷贝）。
func (rc *RuntimeConfig) Get() map[string]any {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.snapshotLocked()
}

// Update 应用部分更新（只更新非空/有效字段），并返回更新后的完整配置。
// 返回 changes 与 restartRequired 提示。
func (rc *RuntimeConfig) Update(patch map[string]any) (map[string]any, bool, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if v, ok := patch["admin_password"]; ok {
		if s, isStr := v.(string); isStr && s != "" && s == DefaultAdminPassword {
			return nil, false, ErrDefaultAdminPassword
		}
	}

	restart := false
	getStr := func(k string) (string, bool) {
		v, ok := patch[k]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok && s != ""
	}
	if v, ok := getStr("api_base_url"); ok {
		rc.APIBaseURL = v
	}
	if v, ok := getStr("website_url"); ok {
		rc.WebsiteURL = v
	}
	if v, ok := patch["api_key_auth"]; ok {
		if s, isStr := v.(string); isStr {
			rc.APIKeyAuth = s // 允许清空
		}
	}
	if v, ok := patch["cache_ttl_seconds"]; ok {
		if f, isNum := asInt(v); isNum && f > 0 {
			rc.CacheTTL = f
		}
	}
	if v, ok := patch["log_max_entries"]; ok {
		if f, isNum := asInt(v); isNum && f > 0 {
			rc.LogMax = f
		}
	}
	if v, ok := getStr("listen_addr"); ok {
		rc.ListenAddr = v
		restart = true
	}
	if v, ok := patch["mock_mode"]; ok {
		if b, isBool := v.(bool); isBool && b != rc.MockMode {
			rc.MockMode = b
			restart = true
		}
	}
	if v, ok := getStr("admin_username"); ok {
		if len(v) >= 2 {
			rc.AdminUsername = v
		}
	}
	if v, ok := patch["admin_password"]; ok {
		if s, isStr := v.(string); isStr && s != "" {
			if err := applyNewPassword(rc, s); err != nil {
				return nil, false, err
			}
		}
	}
	if v, ok := getStr("routing_strategy"); ok {
		rc.RoutingStrategy = v
	}
	if v, ok := patch["session_affinity"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.SessionAffinity = &b
		}
	}
	if v, ok := getStr("session_affinity_ttl"); ok {
		rc.SessionAffinityTTL = v
	}
	if v, ok := patch["request_retry"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 {
			rc.RequestRetry = &n
		}
	}
	if v, ok := patch["max_retry_credentials"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 {
			rc.MaxRetryCredentials = &n
		}
	}
	if v, ok := patch["max_retry_interval"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 {
			rc.MaxRetryIntervalSec = &n
		}
	}
	if v, ok := patch["disable_cooling"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.DisableCooling = b
		}
	}
	if v, ok := patch["warm_preference"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.WarmPreference = b
		}
	}
	if v, ok := patch["allow_remote_admin"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.AllowRemoteAdmin = b
		}
	}
	if v, ok := patch["proxy_url"]; ok {
		if s, isStr := v.(string); isStr {
			rc.ProxyURL = s
		}
	}
	if v, ok := patch["model_routes"]; ok {
		if m, ok := asStringMap(v); ok {
			rc.ModelRoutes = m
		}
	}
	if v, ok := getStr("history_compress"); ok {
		rc.HistoryCompress = v
	}
	if v, ok := patch["system_prompt"]; ok {
		if s, isStr := v.(string); isStr {
			rc.SystemPrompt = s // 允许清空（空=回默认，见 PromptText）
		}
	}
	if v, ok := getStr("system_prompt_mode"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "inject", "":
			rc.SystemPromptMode = "inject"
		case "off":
			rc.SystemPromptMode = "off"
		}
	}
	if v, ok := patch["identity_guard_enabled"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.IdentityGuardEnabled = &b
		}
	}
	if v, ok := patch["identity_answer_text"]; ok {
		if s, isStr := v.(string); isStr {
			rc.IdentityAnswerText = s // 允许清空（空=默认答句）
		}
	}
	if v, ok := patch["log_retention_days"]; ok {
		if n, isNum := asInt(v); isNum && n > 0 {
			rc.LogRetentionDays = n
		}
	}
	if v, ok := patch["batch_concurrency"]; ok {
		if n, isNum := asInt(v); isNum && n > 0 {
			if n > 64 {
				n = 64
			}
			rc.BatchConcurrency = n
		}
	}
	if v, ok := patch["account_concurrency"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 && n <= 256 {
			rc.AccountConcurrency = n
		}
	}
	if v, ok := patch["account_concurrency_429"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 && n <= 256 {
			rc.AccountConcurrency429 = n
		}
	}
	if v, ok := patch["gateway_concurrency"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 && n <= 100000 {
			rc.GatewayConcurrency = n
		}
	}
	if v, ok := patch["gateway_queue_sec"]; ok {
		if n, isNum := asInt(v); isNum && n >= 1 && n <= 600 {
			rc.GatewayQueueSec = n
		}
	}
	if v, ok := patch["prewarm_accounts"]; ok {
		if n, isNum := asInt(v); isNum && n >= 0 && n <= 256 {
			rc.PrewarmAccounts = n
		}
	}
	if v, ok := patch["prewarm_interval_sec"]; ok {
		if n, isNum := asInt(v); isNum && n >= 10 && n <= 600 {
			rc.PrewarmIntervalSec = n
		}
	}
	if v, ok := patch["resin_enabled"]; ok {
		if b, isBool := v.(bool); isBool {
			rc.ResinEnabled = b
		}
	}
	if v, ok := patch["resin_url"]; ok {
		if s, isStr := v.(string); isStr {
			rc.ResinURL = strings.TrimRight(strings.TrimSpace(s), "/")
		}
	}
	if v, ok := getStr("resin_platform"); ok {
		rc.ResinPlatform = v
	}
	if err := rc.saveLocked(); err != nil {
		return nil, false, err
	}
	return rc.snapshotLocked(), restart, nil
}

// snapshotLocked 返回当前配置快照（须持写锁）。
func (rc *RuntimeConfig) snapshotLocked() map[string]any {
	return map[string]any{
		"api_base_url":            rc.APIBaseURL,
		"website_url":             rc.WebsiteURL,
		"api_key_auth":            rc.APIKeyAuth,
		"cache_ttl_seconds":       rc.CacheTTL,
		"log_max_entries":         rc.LogMax,
		"listen_addr":             rc.ListenAddr,
		"mock_mode":               rc.MockMode,
		"admin_username":          rc.AdminUsername,
		"must_change_password":    rc.MustChangePassword,
		"routing_strategy":        rc.RoutingStrategy,
		"session_affinity":        rc.sessionAffinityLocked(),
		"model_routes":            rc.ModelRoutes,
		"history_compress":        rc.HistoryCompress,
		"system_prompt":           rc.SystemPrompt,
		"system_prompt_mode":      rc.SystemPromptMode,
		"identity_guard_enabled":  rc.identityGuardLocked(),
		"identity_answer_text":    rc.IdentityAnswerText,
		"max_retry_credentials":   ptrInt(rc.MaxRetryCredentials, 0),
		"max_retry_interval":      ptrInt(rc.MaxRetryIntervalSec, 30),
		"disable_cooling":         rc.DisableCooling,
		"allow_remote_admin":      rc.AllowRemoteAdmin,
		"proxy_url":               rc.ProxyURL,
		"log_retention_days":      rc.LogRetentionDays,
		"batch_concurrency":       rc.batchConcurrencyLocked(),
		"account_concurrency":     rc.accountConcurrencyLocked(),
		"account_concurrency_429": rc.accountConcurrency429Locked(),
		"prewarm_accounts":        rc.prewarmAccountsLocked(),
		"prewarm_interval_sec":    rc.prewarmIntervalSecLocked(),
		"warm_preference":         rc.WarmPreference,
		"resin_enabled":           rc.ResinEnabled,
		"resin_url":               rc.ResinURL,
		"resin_platform":          rc.resinPlatformLocked(),
	}
}

func (rc *RuntimeConfig) sessionAffinityLocked() bool {
	if rc.SessionAffinity == nil {
		return true
	}
	return *rc.SessionAffinity
}

func (rc *RuntimeConfig) batchConcurrencyLocked() int {
	if rc.BatchConcurrency <= 0 {
		return 4
	}
	return rc.BatchConcurrency
}

// accountConcurrencyLocked 原样返回：<=0 表示不限制（下游按"无边"处理）。
func (rc *RuntimeConfig) accountConcurrencyLocked() int {
	return rc.AccountConcurrency
}

func (rc *RuntimeConfig) accountConcurrency429Locked() int {
	return rc.AccountConcurrency429
}

// AccountConcurrencyN 单号并发上限（<=0 = 不限制）；AccountConcurrency429N 429 后降级值（<=0 = 不降级）。
func (rc *RuntimeConfig) AccountConcurrencyN() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.accountConcurrencyLocked()
}

func (rc *RuntimeConfig) AccountConcurrency429N() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.accountConcurrency429Locked()
}

func (rc *RuntimeConfig) GatewayConcurrencyN() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.GatewayConcurrency
}

func (rc *RuntimeConfig) GatewayQueueSecN() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.GatewayQueueSec
}

func (rc *RuntimeConfig) prewarmAccountsLocked() int {
	return rc.PrewarmAccounts
}

func (rc *RuntimeConfig) prewarmIntervalSecLocked() int {
	if rc.PrewarmIntervalSec <= 0 {
		return 60
	}
	return rc.PrewarmIntervalSec
}

// PrewarmAccountsN warm-set 容量（<=0 = 关闭预热）；PrewarmIntervalSecN 巡检间隔秒（<=0 回退 60）。
func (rc *RuntimeConfig) PrewarmAccountsN() int {
	if rc == nil {
		return 16
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.prewarmAccountsLocked()
}

func (rc *RuntimeConfig) PrewarmIntervalSecN() int {
	if rc == nil {
		return 60
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.prewarmIntervalSecLocked()
}

func (rc *RuntimeConfig) resinPlatformLocked() string {
	if rc.ResinPlatform == "" {
		return "Default"
	}
	return rc.ResinPlatform
}

func ptrInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// SchedulerConfig 把热配置映到 scheduler.Config（CPA 数字默认 + Cursor 粘性默认开）。
func (rc *RuntimeConfig) SchedulerConfig() (strategy string, affinity bool, ttl time.Duration, requestRetry, maxCred int, maxWait time.Duration, disableCooling, warmPreference bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	strategy = rc.RoutingStrategy
	if strategy == "" {
		strategy = "round-robin"
	}
	affinity = rc.sessionAffinityLocked()
	ttl = time.Hour
	if d, err := time.ParseDuration(rc.SessionAffinityTTL); err == nil && d > 0 {
		ttl = d
	}
	requestRetry = ptrInt(rc.RequestRetry, 3)
	maxCred = ptrInt(rc.MaxRetryCredentials, 0)
	sec := ptrInt(rc.MaxRetryIntervalSec, 30)
	maxWait = time.Duration(sec) * time.Second
	disableCooling = rc.DisableCooling
	warmPreference = rc.WarmPreference
	return
}

// DefaultAdminUsername / DefaultAdminPassword 首次启动写入的默认管理员凭据。
const (
	DefaultAdminUsername = "admin"
	DefaultAdminPassword = "admin123"
)

// ErrDefaultAdminPassword 拒绝把密码设回默认 admin123。
var ErrDefaultAdminPassword = errors.New("password cannot be the default admin123")

// SetPassword 设置管理员密码（PBKDF2-HMAC-SHA256 加盐哈希）。不校验默认值；改密请用 ChangePassword。
func (rc *RuntimeConfig) SetPassword(pw string) {
	rc.AdminPasswordHash = hashPassword(pw)
}

func (rc *RuntimeConfig) applyDefaultPassword() {
	rc.AdminPasswordHash = hashPassword(DefaultAdminPassword)
	rc.MustChangePassword = true
}

func applyNewPassword(rc *RuntimeConfig, pw string) error {
	if pw == "" {
		return errors.New("password is required")
	}
	if pw == DefaultAdminPassword {
		return ErrDefaultAdminPassword
	}
	rc.AdminPasswordHash = hashPassword(pw)
	rc.MustChangePassword = false
	return nil
}

func (rc *RuntimeConfig) isDefaultPassword() bool {
	if rc.AdminPasswordHash == "" {
		return false
	}
	return verifyPassword(rc.AdminPasswordHash, DefaultAdminPassword)
}

// ChangePassword 管理员改密：拒绝 admin123，成功后清除 must_change_password 并落盘。
func (rc *RuntimeConfig) ChangePassword(pw string) error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if err := applyNewPassword(rc, pw); err != nil {
		return err
	}
	return rc.saveLocked()
}

// MustChange 是否仍使用默认密码、其它管理 API 应被拦截。
func (rc *RuntimeConfig) MustChange() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.MustChangePassword
}

// PublicAPIKey 返回运行时 api_key_auth（热配置，空 = 不鉴权）。
func (rc *RuntimeConfig) PublicAPIKey() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.APIKeyAuth
}

// CheckPassword 校验管理员密码。
func (rc *RuntimeConfig) CheckPassword(pw string) bool {
	rc.mu.RLock()
	stored := rc.AdminPasswordHash
	rc.mu.RUnlock()
	if stored == "" {
		return false
	}
	return verifyPassword(stored, pw)
}

// ---- PBKDF2-HMAC-SHA256（纯标准库实现，避免第三方依赖） ----

const (
	pbkdf2Iterations = 200_000
	pbkdf2SaltLen    = 16
)

// hashPassword 生成 "pbkdf2-sha256$iter$salt_hex$hash_hex" 格式的哈希。
func hashPassword(pw string) string {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err) // crypto/rand 失败无法继续
	}
	dk := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iterations, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(dk))
}

// verifyPassword 校验密码与存储哈希是否匹配。
func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(expected))
	return subtle.ConstantTimeCompare(got, expected) == 1
}

// pbkdf2SHA256 标准 PBKDF2（RFC 2898）核心实现。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	var u []byte
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		var b [4]byte
		b[0] = byte(block >> 24)
		b[1] = byte(block >> 16)
		b[2] = byte(block >> 8)
		b[3] = byte(block)
		prf.Write(b[:])
		u = prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// TimeNow 返回当前时间（便于测试替换）。
var TimeNow = time.Now

// AllowRemote 是否允许非本机访问管理面。
func (rc *RuntimeConfig) AllowRemote() bool {
	if rc == nil {
		return false
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.AllowRemoteAdmin
}

// applyAllowRemoteFromEnv 读取 WEB2API_ALLOW_REMOTE_ADMIN（true/1/yes）。
// 未设置则不动；返回是否改写了字段。
func applyAllowRemoteFromEnv(rc *RuntimeConfig) bool {
	if rc == nil {
		return false
	}
	v := strings.TrimSpace(os.Getenv("WEB2API_ALLOW_REMOTE_ADMIN"))
	if v == "" {
		return false
	}
	on := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	if rc.AllowRemoteAdmin == on {
		return false
	}
	rc.AllowRemoteAdmin = on
	return true
}

// GlobalProxy 全局上游代理。
func (rc *RuntimeConfig) GlobalProxy() string {
	if rc == nil {
		return ""
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.ProxyURL
}

// ModelRouteMap 模型路由表拷贝。
func (rc *RuntimeConfig) ModelRouteMap() map[string]string {
	if rc == nil {
		return nil
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if len(rc.ModelRoutes) == 0 {
		return nil
	}
	out := make(map[string]string, len(rc.ModelRoutes))
	for k, v := range rc.ModelRoutes {
		out[k] = v
	}
	return out
}

// HistoryTier 历史压缩档位。
func (rc *RuntimeConfig) HistoryTier() string {
	if rc == nil {
		return "code"
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if rc.HistoryCompress == "" {
		return "code"
	}
	return rc.HistoryCompress
}

// PromptEnabled 注入总开关。mode=off 关闭；默认注入。
func (rc *RuntimeConfig) PromptEnabled() bool {
	if rc == nil {
		return true
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return !strings.EqualFold(strings.TrimSpace(rc.SystemPromptMode), "off")
}

// PromptText 返回本轮要注入的指令（空串=不注入，由调用方判断）。
// 管理端文案优先；空则回默认常量（默认常量在 adapter 侧，见 prompt.go）。
func (rc *RuntimeConfig) PromptText() string {
	if rc == nil || !rc.PromptEnabled() {
		return ""
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.SystemPrompt
}

// IdentityGuardOn 身份闸总开关（nil=默认开）。
func (rc *RuntimeConfig) IdentityGuardOn() bool {
	if rc == nil {
		return true
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.identityGuardLocked()
}

func (rc *RuntimeConfig) identityGuardLocked() bool {
	if rc.IdentityGuardEnabled == nil {
		return true
	}
	return *rc.IdentityGuardEnabled
}

// IdentityAnswer 身份闸回退答句（空串=默认答句，默认答句在 api 侧常量）。
func (rc *RuntimeConfig) IdentityAnswer() string {
	if rc == nil {
		return ""
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return strings.TrimSpace(rc.IdentityAnswerText)
}

// RetentionDays PostgreSQL 追踪保留天数。
func (rc *RuntimeConfig) RetentionDays() int {
	if rc == nil {
		return 7
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if rc.LogRetentionDays <= 0 {
		return 7
	}
	return rc.LogRetentionDays
}

// BatchConcurrencyN 批量 CRUD / 任务并发上限。
func (rc *RuntimeConfig) BatchConcurrencyN() int {
	if rc == nil {
		return 4
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.batchConcurrencyLocked()
}

// ResinOn 是否启用全局 Resin 出口（账号未绑代理池时）。
func (rc *RuntimeConfig) ResinOn() bool {
	if rc == nil {
		return false
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.ResinEnabled && strings.TrimSpace(rc.ResinURL) != ""
}

// ResinEndpoint 全局 Resin 根地址。
func (rc *RuntimeConfig) ResinEndpoint() (url, platform string) {
	if rc == nil {
		return "", "Default"
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.ResinURL, rc.resinPlatformLocked()
}

func unmarshalConfig(data []byte, dst *RuntimeConfig) error {
	t := strings.TrimSpace(string(data))
	if t == "" {
		return nil
	}
	if t[0] == '{' {
		return json.Unmarshal(data, dst)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// prewarmKeyPresent 判断持久化文档是否显式出现某 key：JSON 按顶层键查，
// YAML 先转 map 再查；都不是则回退为"出现"（不回填默认，防误覆盖）。
// 目的：老库缺字段回填默认 16/90，而显式 0（关闭/回退取值）必须原样保留。
func prewarmKeyPresent(data []byte, key string) bool {
	t := strings.TrimSpace(string(data))
	if t == "" {
		return false
	}
	if t[0] == '{' {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return true
		}
		_, ok := m[key]
		return ok
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return true
	}
	_, ok := m[key]
	return ok
}

func asStringMap(v any) (map[string]string, bool) {
	switch m := v.(type) {
	case map[string]string:
		out := make(map[string]string, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out, true
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, val := range m {
			s, ok := val.(string)
			if !ok {
				continue
			}
			out[k] = s
		}
		return out, true
	default:
		return nil, false
	}
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
