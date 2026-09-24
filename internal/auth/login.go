// Package auth 管理令牌与 PKCE。Vendor 登录/换票走 Auth hooks（adapter.Bind）。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Token 是上游访问令牌与刷新令牌。
type Token struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	APIKey       string `json:"api_key,omitempty"` // 关联的 API Key（用于重启后自动刷新）
}

// ErrInvalidAPIKey 表示 API Key 无效（服务端 401/403）。
var ErrInvalidAPIKey = errors.New("invalid user api key")

// GeneratePKCE 生成 PKCE verifier 与 challenge（S256）。
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// LoginURL 构造 loginDeepControl 登录地址。
func LoginURL(websiteURL, challenge, uuid string) string {
	return LoginURLHook(websiteURL, challenge, uuid)
}

// ExchangeAPIKey 使用 API Key 换取访问令牌（实现由 adapter Auth hook 提供）。
func ExchangeAPIKey(apiBaseURL, apiKey, clientVersion, clientType string, client *http.Client) (*Token, error) {
	return ExchangeHook(apiBaseURL, apiKey, clientVersion, clientType, client)
}

// PollResult 轮询登录结果（实现由 adapter Auth hook 提供）。
func PollResult(apiBaseURL, uuid, verifier, clientVersion, clientType string, client *http.Client) (*Token, error) {
	return PollHook(apiBaseURL, uuid, verifier, clientVersion, clientType, client)
}

// ErrSessionPending is returned by a site PollLogin while the browser session is not ready.
var ErrSessionPending = errSessionPending

var errSessionPending = errors.New("session pending")

// IsSessionPending 判断错误是否为"会话未就绪"（应继续轮询）。
func IsSessionPending(err error) bool {
	return errors.Is(err, errSessionPending)
}

func jwtPayload(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.RawStdEncoding.DecodeString(parts[1])
	}
	if err != nil {
		payload, err = base64.StdEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil
	}
	return payload
}

// JWTExpiry 解析 JWT 的 exp 字段（unix 秒）。解析失败返回 0。
func JWTExpiry(token string) int64 {
	payload := jwtPayload(token)
	if payload == nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0
	}
	return claims.Exp
}

// JWTSubject 解析 JWT 的 sub。解析失败返回空串。
func JWTSubject(token string) string {
	payload := jwtPayload(token)
	if payload == nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.TrimSpace(claims.Sub)
}

// JWTEmail 从 JWT 载荷里取邮箱：顶层 email，或任意一层嵌套对象里的 email
// （各家把用户资料放在 profile / user / account 等不同命名空间下）。
// 解析失败返回空串。
func JWTEmail(token string) string {
	payload := jwtPayload(token)
	if payload == nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return findEmail(claims)
}

func findEmail(node any) string {
	switch v := node.(type) {
	case map[string]any:
		if s, ok := v["email"].(string); ok && strings.Contains(s, "@") {
			return strings.TrimSpace(s)
		}
		for _, child := range v {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	case []any:
		for _, child := range v {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	}
	return ""
}

// TokenManager 管理令牌的获取、缓存与刷新。
type TokenManager struct {
	mu            sync.Mutex
	token         *Token
	apiKey        string
	apiBaseURL    string
	clientVersion string
	clientType    string
	client        *http.Client
	onRefresh     func(*Token) // 刷新成功后回调（用于持久化）
	// refreshFlight 合并并发 Refresh 网络请求：实例粒度为单账号，
	// key 只需区分 kind（当前仅 "refresh"），10 路同时过期只出 1 次网络。
	refreshFlight singleflight.Group
}

func NewTokenManager(apiBaseURL, apiKey, clientVersion, clientType string) *TokenManager {
	return &TokenManager{
		apiKey:        apiKey,
		apiBaseURL:    apiBaseURL,
		clientVersion: clientVersion,
		clientType:    clientType,
		client:        &http.Client{Timeout: 30 * time.Second},
	}
}

// SetOnRefresh 注册刷新成功后的回调（如持久化到磁盘）。
func (m *TokenManager) SetOnRefresh(fn func(*Token)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRefresh = fn
}

// SetHTTPClient 替换令牌刷新所用的 HTTP 客户端（出口代理 / Resin）。
func (m *TokenManager) SetHTTPClient(c *http.Client) {
	if m == nil || c == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.client = c
}

// SetBaseURL 更新 API 地址（如 mock 模式下切换后端）。
func (m *TokenManager) SetBaseURL(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiBaseURL = url
}

// SetToken 手动注入令牌（如从凭据文件加载）。
func (m *TokenManager) SetToken(t *Token) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = t
	if t != nil && t.APIKey != "" && m.apiKey == "" {
		m.apiKey = t.APIKey // 从持久化恢复 API Key 模式
	}
}

// Current 返回当前令牌（不触发刷新）；无令牌时返回 nil。
func (m *TokenManager) Current() *Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == nil {
		return nil
	}
	cp := *m.token
	return &cp
}

// TokenLocal 仅本地判断：access token 在宽限期内有效则返回，否则 nil——
// 绝不触发网络刷新。选号热路径（ReadyWithLimits）用它；无效号被直接跳过，
// 刷新交给保活循环（曾因选号路径同步 relogin 卡死调度器，pick 实测挂 23 分钟）。
func (m *TokenManager) TokenLocal() *Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == nil || m.token.AccessToken == "" || isExpired(m.token.AccessToken) {
		return nil
	}
	cp := *m.token
	return &cp
}

// DropAccessToken 作废本地 access token（上游 403=session 吊销时调用）：
// 号立即退出选号（TokenLocal=nil），但 RefreshToken（重登凭据束）完整保留，
// 保活循环随后重登；成功即满血回池。不触发任何网络请求。
func (m *TokenManager) DropAccessToken() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == nil || m.token.AccessToken == "" {
		return
	}
	m.token.AccessToken = ""
}

// HasAPIKey 判断是否配置了 API Key 模式。
func (m *TokenManager) HasAPIKey() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiKey != ""
}

// APIKey 返回当前 API Key（可能为空）。
func (m *TokenManager) APIKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiKey
}

// Token 返回当前令牌；若无有效令牌且配置了 API Key 则自动换取。
func (m *TokenManager) Token() (*Token, error) {
	m.mu.Lock()
	if m.token != nil && m.token.AccessToken != "" && !isExpired(m.token.AccessToken) {
		tok := m.token
		m.mu.Unlock()
		return tok, nil
	}
	m.mu.Unlock()
	return m.Refresh()
}

// HasRefreshToken 是否持有 refresh token（无则后台保活跳过）。
func (m *TokenManager) HasRefreshToken() bool {
	t := m.Current()
	return t != nil && t.RefreshToken != ""
}

// Refresh 强制走刷新路径（忽略本地 JWT 是否仍在 5 分钟宽限内）。
// 后台保活在过期前 15 分钟调用；请求路径 Token() 仍只在即将过期时调用。
func (m *TokenManager) Refresh() (*Token, error) {
	m.mu.Lock()
	if m.apiKey == "" && (m.token == nil || m.token.RefreshToken == "") {
		m.mu.Unlock()
		if m.token != nil {
			return nil, errors.New("access token expired and no refresh token or api key available")
		}
		return nil, errors.New("not logged in")
	}
	// 快照后释放锁：网络请求与 onRefresh 回调在锁外执行（避免回调内取锁死锁）
	// API Key 账号用 apiKey 换新令牌；OAuth 账号用 refreshToken 换新令牌（同一端点，与官方 CLI/oh-my-pi 一致）。
	secret := m.apiKey
	if secret == "" {
		secret = m.token.RefreshToken
	}
	apiBaseURL, cv, ct, client := m.apiBaseURL, m.clientVersion, m.clientType, m.client
	m.mu.Unlock()

	// 并发合并：同一实例（单账号）多路同时 Refresh 只出 1 次网络，
	// 等待方共享 leader 的结果（含 store+onRefresh，只执行一次）。
	v, err, _ := m.refreshFlight.Do("refresh", func() (any, error) {
		tok, err := ExchangeHook(apiBaseURL, secret, cv, ct, client)
		if err != nil {
			log.Printf("token refresh via %s failed: %v", apiBaseURL, err)
			return nil, fmt.Errorf("auto refresh token: %w", err)
		}
		m.mu.Lock()
		m.token = tok
		cb := m.onRefresh
		m.mu.Unlock()
		if cb != nil {
			cb(tok)
		}
		return tok, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Token), nil
}

// isExpired 判断 JWT 是否已过期（提前 300 秒视为过期，与官方 CLI 一致）。
func isExpired(token string) bool {
	exp := JWTExpiry(token)
	if exp == 0 {
		return false // 无法解析则不判过期
	}
	return time.Now().Unix() >= exp-300
}

// 同步原语（见 TokenManager.mu）。

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
