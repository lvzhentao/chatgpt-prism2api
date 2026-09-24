package prism

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"prism-2api/internal/adapter"
	"prism-2api/internal/auth"
	"prism-2api/internal/egress"
)

// ============================================================
// prism 客户端：账号级状态机
//
// 认证：两个 cookie。客户端只从内核拿到一个令牌串
// （prism_oai_access_token），session cookie 由自己用
// POST /auth/session 自举 / 续期（真机验证：只带 oai cookie 即可）。
//
// 一轮对话需要先「预热沙箱」，否则 start 会无限挂起：
//   backend/1/new → projects/{id}/sandbox/resources-token
//   → s/sandboxes/proxy/resources-token → api/y → s/sandboxes/proxy/token
//   → wait-for-sync(status=synced)
// 预热结果按账号缓存，闲置超时或 start 超时后自动重来。
// ============================================================

const (
	projectTitle = "prism-2api"

	// 沙箱闲置上限。真机实测（2026-09-17）：即使每 60s 打一次廉价 ping，沙箱会话也在
	// **5 分钟内**失效（第 5 次 ping 401），而拿过期 token 提交 start 会**挂死**（实测 90s 无响应），
	// 比重新预热（6~18s）糟得多。所以 TTL 取 2 分钟，宁可贵一点也别撞死沙箱。
	sandboxIdleTTL  = 2 * time.Minute
	sessionGrace    = 5 * time.Minute  // session cookie 提前续期窗口
	startTimeout    = 75 * time.Second // 单次 start 尝试上限（未预热会挂死）
	pollCallTimeout = 30 * time.Second
	syncPollTries   = 4
	registerTimeout = 5 * time.Second // register 独立超时（detached，不随请求 ctx 取消）
	httpUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0"
)

// 节奏参数：生产值即上游实测值，测试里调小以免每个用例白等好几秒。
var (
	// 沙箱刚建好时第一次 start 常回 sandbox_reconnecting（工作区还在 sync）。
	// 参考实现 prism-proxy 的做法是「等 codex 就绪后重试同一个沙箱」——
	// 我们原先直接换沙箱，等于白付一次 6~18s 预热。
	sandboxReconnectWait = 1500 * time.Millisecond
	maxSandboxReconnects = 3
	codexReadyBudget     = 12 * time.Second
	codexReadyPoll       = time.Second
)

// upstreamError 让 failclass 能按状态码分类（401/403 → 换号，429 → 降并发）。
type upstreamError struct {
	Status int
	Msg    string
}

func (e *upstreamError) Error() string {
	if e.Status <= 0 {
		return "prism: " + e.Msg
	}
	return fmt.Sprintf("prism: status %d: %s", e.Status, e.Msg)
}
func (e *upstreamError) StatusCode() int { return e.Status }

// retryable 判断这次失败值不值得换沙箱重试：4xx 是请求过错（模型名不在账号清单里、
// 上下文超长），与沙箱/会话无关，重试只会白等 startTimeout；5xx 与未知状态才是上游抖动。
func (e *upstreamError) retryable() bool { return !(e.Status >= 400 && e.Status < 500) }

// upstreamStatusRe 从上游内联报错里抽状态码。文案形如
// "Error while processing conversation (400 Bad Request). Please submit prompt again."
var upstreamStatusRe = regexp.MustCompile(`\((\d{3})\s+[A-Za-z][A-Za-z ]*\)`)

// startFailedError 包装「HTTP 200 + 内联报错」的失败，并尽力带上真实状态码。
// 状态码是这里唯一的线索：模型名不对时上游只在这一句里写 400，抽不出来就会
// 被 failclass 当 server 类处理 —— 整账号冷却 2 分钟，客户端也拿到 502 而非 400。
func startFailedError(msg string) *upstreamError {
	if m := upstreamStatusRe.FindStringSubmatch(msg); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 100 && n <= 599 {
			return &upstreamError{Status: n, Msg: msg}
		}
	}
	return &upstreamError{Msg: msg}
}

// credential 是上游认证材料。
type credential struct {
	OAI     string // prism_oai_access_token（必需，10 天）
	Session string // prism_session_token（可选，缺则由 /auth/session 自举）
}

type httpClient struct {
	baseURL       string
	clientVersion string
	clientType    string
	token         func() (string, error)
	http          *http.Client
	prepared      bool // 上次 prepareSandbox 是否成功（失败则同步给默认客户端）

	// flight 合并同账号并发冷链（T2.2）：key "session"/"sandbox"。
	// httpClient 实例已是 per-account（pool 按账号 NewClient），组内即按账号隔离；
	// 请求路径与 Prewarm 共享同一 flight。
	flight singleflight.Group

	mu         sync.Mutex
	cred       credential
	credLoaded bool
	session    string
	sessionExp int64
	userID     string
	email      string
	projectID  string
	sandbox    string
	sandboxURL string
	sandboxAt  time.Time
	// codexHealthzUnsupported：{sandbox}/codex/healthz 不存在时不反复白等（见 waitCodexReady）。
	codexHealthzUnsupported bool
}

func newHTTPClient(cfg adapter.ClientConfig) *httpClient {
	return &httpClient{
		baseURL:       cfg.BaseURL,
		clientVersion: cfg.ClientVersion,
		clientType:    cfg.ClientType,
		token:         cfg.TokenProvider,
		http:          &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *httpClient) SetBaseURL(url string) {
	if strings.TrimSpace(url) != "" {
		c.baseURL = url
	}
}

func (c *httpClient) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.mu.Lock()
		c.http = h
		c.mu.Unlock()
	}
}

// SetEgress 按出口设置重建 HTTP 客户端（Resin 由 Transport 内部改写 URL）。
func (c *httpClient) SetEgress(s egress.Settings) error {
	if s.Account == "" {
		s.Account = "prism"
	}
	cli, err := egress.HTTPClient(s, 120*time.Second)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.http = cli
	c.mu.Unlock()
	return nil
}

func (c *httpClient) httpClient() *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.http
}

// ---------------------------------------------------------------- 凭据

// loadCredential 从内核令牌提供者取凭据并缓存（令牌串变化时重取）。
func (c *httpClient) loadCredential() (credential, error) {
	if c.token == nil {
		return credential{}, fmt.Errorf("prism: no credential configured")
	}
	raw, err := c.token()
	if err != nil {
		return credential{}, err
	}
	cred := parseCredential(raw)
	if cred.OAI == "" {
		return credential{}, fmt.Errorf("prism: credential is empty")
	}
	c.mu.Lock()
	if cred != c.cred {
		c.cred = cred
		c.session = cred.Session // 导入时若带了 session cookie 就直接用
		c.sessionExp = auth.JWTExpiry(cred.Session)
	}
	c.mu.Unlock()
	return cred, nil
}

// parseCredential 接受三种形态：裸 oai JWT、Cookie 头、JSON。
func parseCredential(raw string) credential {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return credential{}
	}
	pick := func(s, name string) string {
		i := strings.Index(s, name+"=")
		if i < 0 {
			return ""
		}
		v := s[i+len(name)+1:]
		if j := strings.IndexAny(v, ";\"\n\r\t ,}"); j >= 0 {
			v = v[:j]
		}
		return strings.TrimSpace(v)
	}
	switch {
	case strings.Contains(raw, "prism_oai_access_token=") || strings.Contains(raw, "prism_session_token="):
		return credential{OAI: pick(raw, "prism_oai_access_token"), Session: pick(raw, "prism_session_token")}
	case strings.HasPrefix(raw, "{"):
		var v map[string]any
		if json.Unmarshal([]byte(raw), &v) == nil {
			str := func(keys ...string) string {
				for _, k := range keys {
					if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
				return ""
			}
			cred := credential{
				OAI:     str("prism_oai_access_token", "access_token", "oai_access_token", "token", "api_key"),
				Session: str("prism_session_token", "session_token"),
			}
			if cred.OAI == "" || cred.Session == "" {
				// 整份 Cookie-Editor 导出：cookies[].name/value
				if arr, ok := v["cookies"].([]any); ok {
					for _, it := range arr {
						m, _ := it.(map[string]any)
						n, _ := m["name"].(string)
						val, _ := m["value"].(string)
						switch n {
						case "prism_oai_access_token":
							cred.OAI = val
						case "prism_session_token":
							cred.Session = val
						}
					}
				}
			}
			return cred
		}
	}
	return credential{OAI: raw}
}

// ---------------------------------------------------------------- HTTP

type apiResponse struct {
	status  int
	body    []byte
	header  http.Header
	cookies []*http.Cookie
	// reused 是本次请求是否复用了空闲连接（T0.3：httptrace GotConn.Reused）。
	reused bool
}

func (r *apiResponse) json() map[string]any {
	var v map[string]any
	_ = json.Unmarshal(r.body, &v)
	return v
}

// request 发一次带 cookie 的上游请求。body 为 nil 时不带请求体。
func (c *httpClient) request(ctx context.Context, method, url string, body any, headers map[string]string) (*apiResponse, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	// T0.3：捕获连接复用情况，带入诊断日志（conn_reused）。
	var reused bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", c.origin())
	req.Header.Set("Referer", c.origin()+"/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ck := c.cookieHeader(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 上游验证扩面（2026-09-20 晚实锤）：沙箱/会话面（backend/1/new、
	// resources-token、conversation-history）也开始强制 sentinel——裸发一律
	// 403 "Request verification failed"，全池预热瘫痪一夜。这些路径自动补
	// 一次性 sentinel 票 + 材料浏览器 cookie（对话 start 由 startTurn 手动注入，
	// 这里跳过避免双份）。
	if !strings.Contains(url, PathChat) && upstreamVerifiedPath(url) {
		if tok, terr := takeSentinelToken(ctx); terr == nil {
			req.Header.Set("openai-sentinel-token", tok)
		}
		if ck := currentMaterialCookie(); ck != "" {
			req.Header.Set("Cookie", ck)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return &apiResponse{status: resp.StatusCode, body: raw, header: resp.Header, cookies: resp.Cookies(), reused: reused}, nil
}

// upstreamVerifiedPath 判断 URL 是否落在上游强制验证面。2026-09-21 起上游
// 把验证从对话面一路扩到沙箱/项目/会话历史（projects 也 403，实测票全严格
// 一次性、材料 cookie 单独无效）——与其逐路径追补，除 auth 自举外全部 /api/
// 面默认带票；上游再扩面零改动。
func upstreamVerifiedPath(url string) bool {
	return strings.Contains(url, "/api/") && !strings.Contains(url, "/auth/")
}

// requestRetry 对瞬时故障（5xx / 429 / 网络抖动）自行重试，
// 避免把上游抽风透给内核的失败分类（5xx 会给账号 2 分钟冷却）。
// 4xx 直接返回给调用方判定。
func (c *httpClient) requestRetry(ctx context.Context, method, url string, body any, headers map[string]string, attempts int) (*apiResponse, error) {
	if attempts < 1 {
		attempts = 1
	}
	var (
		lastResp *apiResponse
		lastErr  error
	)
	for i := 0; i < attempts; i++ {
		if i > 0 {
			sleepCtx(ctx, time.Duration(i)*800*time.Millisecond)
			if ctx.Err() != nil {
				break
			}
		}
		resp, err := c.request(ctx, method, url, body, headers)
		if err != nil {
			// backend 劣化（预热预算耗尽）：重试只是白等，直接把原因交回调用方。
			if isDegraded(err) {
				return resp, err
			}
			lastErr, lastResp = err, nil
			continue
		}
		if resp.status >= 500 || resp.status == http.StatusTooManyRequests {
			lastResp, lastErr = resp, nil
			continue
		}
		return resp, nil
	}
	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func (c *httpClient) origin() string {
	if c.baseURL != "" {
		return strings.TrimRight(c.baseURL, "/")
	}
	return "https://prism.openai.com"
}

func (c *httpClient) cookieHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	if c.cred.OAI != "" {
		parts = append(parts, "prism_oai_access_token="+c.cred.OAI)
	}
	if c.session != "" {
		parts = append(parts, "prism_session_token="+c.session)
	}
	return strings.Join(parts, "; ")
}

func (c *httpClient) setSessionFrom(resp *apiResponse) {
	for _, ck := range resp.cookies {
		if ck.Name != "prism_session_token" || ck.Value == "" {
			continue
		}
		c.mu.Lock()
		c.session = ck.Value
		c.sessionExp = auth.JWTExpiry(ck.Value)
		c.mu.Unlock()
	}
}

func (c *httpClient) sessionValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == "" {
		return false
	}
	if c.sessionExp == 0 {
		return true // 解析不出 exp 就当有效，靠 401 兜底
	}
	return time.Now().Unix() < c.sessionExp-int64(sessionGrace.Seconds())
}

// ensureSession 自举 / 续期 prism_session_token。
// T2.2：并发冷启动经包内 flight 合并（key "session"；实例已 per-account），
// 请求路径与 Prewarm 共享同一 flight：在飞的预热直接等结果，不重复打 /auth/session。
// follower 经 DoChan 等 leader：Do 不感知 context，预算 ctx 过期也拦不住 follower
// 一直等到 leader 跑完；select ctx.Done() 让调用方一到点就立即返回，不再陪跑。
// （leader 仍用自己的 ctx 把链跑完，属可接受的后台浪费——调用方不等它。）
func (c *httpClient) ensureSession(ctx context.Context) error {
	if c.sessionValid() {
		return nil
	}
	ch := c.flight.DoChan("session", func() (any, error) {
		if c.sessionValid() {
			return nil, nil
		}
		resp, err := c.requestRetry(ctx, http.MethodPost, c.origin()+PathSession, nil, nil, 3)
		if err != nil {
			return nil, fmt.Errorf("prism: mint session: %w", err)
		}
		if resp.status >= 300 {
			// edge→origin 的 502/503/504 是上游整体不可用（劣化期实测 session 铸造被 Cloudflare 504），
			// 换账号打的是同一个 origin，重试只是白等。
			if de := degradedFrom(resp.status, resp.body); de != nil {
				return nil, de
			}
			return nil, &upstreamError{Status: resp.status, Msg: "auth/session: " + truncate(string(resp.body), 200)}
		}
		c.setSessionFrom(resp)
		c.absorbAccount(resp.json())
		if !c.sessionValid() {
			return nil, fmt.Errorf("prism: auth/session did not return a session cookie")
		}
		return nil, nil
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// absorbAccount 从 /auth/session 响应里取账号身份。
func (c *httpClient) absorbAccount(v map[string]any) {
	if v == nil {
		return
	}
	user, _ := v["user"].(map[string]any)
	if user == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userID == "" {
		if meta, ok := user["app_metadata"].(map[string]any); ok {
			c.userID, _ = meta["user_id"].(string)
		}
	}
	if c.email == "" {
		c.email, _ = user["email"].(string)
	}
}

func (c *httpClient) accountID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
}

// ---------------------------------------------------------------- 项目

// ensureProject 返回该账号用于 API 对话的项目 id（没有就建一个）。
func (c *httpClient) ensureProject(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.projectID != "" {
		id := c.projectID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	resp, err := c.requestRetry(ctx, http.MethodGet, c.origin()+PathProjectList, nil, nil, 3)
	if err != nil {
		return "", fmt.Errorf("prism: list projects: %w", err)
	}
	if resp.status >= 300 {
		return "", &upstreamError{Status: resp.status, Msg: "projects: " + truncate(string(resp.body), 200)}
	}
	if projects, ok := resp.json()["projects"].([]any); ok {
		for _, it := range projects {
			m, _ := it.(map[string]any)
			if title, _ := m["title"].(string); title == projectTitle {
				if id, _ := m["uuid"].(string); id != "" {
					c.setProject(id)
					return id, nil
				}
			}
		}
	}

	id := uuid4()
	resp, err = c.requestRetry(ctx, http.MethodPost, c.origin()+PathProjects, map[string]any{
		"project_uuid": id,
		"title":        projectTitle,
		"file_uuids":   []any{},
	}, nil, 3)
	if err != nil {
		return "", fmt.Errorf("prism: create project: %w", err)
	}
	if resp.status >= 300 {
		return "", &upstreamError{Status: resp.status, Msg: "create project: " + truncate(string(resp.body), 200)}
	}
	if got, _ := resp.json()["uuid"].(string); got != "" {
		id = got
	}
	c.setProject(id)
	return id, nil
}

func (c *httpClient) setProject(id string) {
	c.mu.Lock()
	c.projectID = id
	c.mu.Unlock()
}

// ---------------------------------------------------------------- 沙箱

func (c *httpClient) sandboxToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sandbox != "" && time.Since(c.sandboxAt) < sandboxIdleTTL {
		return c.sandbox
	}
	return ""
}

func (c *httpClient) invalidateSandbox() {
	c.mu.Lock()
	c.sandbox = ""
	c.sandboxURL = ""
	c.sandboxAt = time.Time{}
	projectID := c.projectID
	c.mu.Unlock()
	// 清空后异步立即重铸：与请求路径共用 "sandbox" flight，在飞的铸造自动合并去重，
	// 把「失效 → 下轮 prewarm sweep」最长 90s 的冷窗压到一次铸造耗时（~8s）。
	// 风暴开闸时跳过重铸：全池连环 invalidate 的重铸风暴（线上实测 215 次/60s）
	// 只会给已经挂死的上游补刀；沙箱已清空，风暴过后下轮 sweep/请求路径自然重建。
	if projectID == "" || adapter.StormOpen() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := c.prepareSandbox(ctx, projectID); err != nil {
			log.Printf("prism: sandbox recast after invalidate: %v", err)
			return
		}
		// 成功也打一行：F1 是否生效只能靠日志验证，失败路径有日志而成功没有等于盲飞。
		log.Printf("prism: sandbox recast after invalidate ok")
	}()
}

// ensureSandbox 返回可用沙箱令牌；过期或首次调用会走完整预热链。
func (c *httpClient) ensureSandbox(ctx context.Context, projectID string) (string, error) {
	if tok := c.sandboxToken(); tok != "" {
		return tok, nil
	}
	return c.prepareSandbox(ctx, projectID)
}

// sandboxBaseURL 返回沙箱代理基址（prepareSandbox 时记下来的那个）。
func (c *httpClient) sandboxBaseURL() string {
	c.mu.Lock()
	base := c.sandboxURL
	c.mu.Unlock()
	if strings.TrimSpace(base) != "" {
		return strings.TrimRight(base, "/")
	}
	return strings.TrimRight(c.origin(), "/") + PathSandboxBase
}

// isSandboxReconnecting 识别"沙箱还没就绪"这种可等可重试的失败。
func isSandboxReconnecting(v map[string]any, msg string) bool {
	if reason := stringAt(mapAt(mapAt(v, "response"), "payload"), "reason"); reason == "sandbox_reconnecting" {
		return true
	}
	return strings.Contains(msg, "sandbox_reconnecting") || strings.Contains(msg, "Reconnecting to sandbox")
}

// waitCodexReady 等沙箱里的 codex 后端就绪（参考实现 prism-proxy 用的 {sandbox}/codex/healthz）。
//
// 端点不存在/401/404 时**记下来别再试**：它只是加速手段，不是必需条件，
// 而每次白等满预算是要付代价的（reconnecting 重试路径上）。
func (c *httpClient) waitCodexReady(ctx context.Context, sandboxToken string) {
	c.mu.Lock()
	if c.codexHealthzUnsupported {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	deadline := time.Now().Add(codexReadyBudget)
	for time.Now().Before(deadline) {
		resp, err := c.request(ctx, http.MethodGet, c.sandboxBaseURL()+"/codex/healthz", nil,
			map[string]string{"X-Crixet-Sandbox-Token": sandboxToken})
		if err == nil {
			if resp.status == http.StatusOK && stringAt(resp.json(), "status") == "ok" {
				return
			}
			if resp.status == http.StatusNotFound || resp.status == http.StatusUnauthorized ||
				resp.status == http.StatusForbidden {
				c.mu.Lock()
				c.codexHealthzUnsupported = true
				c.mu.Unlock()
				log.Printf("prism: {sandbox}/codex/healthz 不可用（HTTP %d），后续不再探测", resp.status)
				return
			}
		}
		sleepCtx(ctx, codexReadyPoll)
	}
}

// prepareSandbox 预热链：任何一步失败都会让 start 挂起，所以必须全部完成。
// T2.2：并发冷启动经包内 flight 合并（key "sandbox"；实例已 per-account），
// 请求路径与 Prewarm 共享同一 flight：在飞的预热直接等结果，不重复打 backend/1/new。
// follower 经 DoChan 等 leader：Do 不感知 context，预算 ctx 过期也拦不住 follower
// 一直等到 leader 跑完；select ctx.Done() 让预算一到就立即返回，不再陪跑。
//
// 整条链套总预算（见 degraded.go）：上游 backend 劣化时快速失败，不挂死几分钟。
func (c *httpClient) prepareSandbox(ctx context.Context, projectID string) (string, error) {
	return c.withWarmupBudget(ctx, func(ctx context.Context) (string, error) {
		if tok := c.sandboxToken(); tok != "" {
			return tok, nil
		}
		// 代际保护的入 flight 快照：发布时只有当 sandboxAt 不比它新才写入，
		// 另一条 flight（prewarm-sandbox）先发布了更新令牌时我们丢弃自己的旧令牌。
		c.mu.Lock()
		entryAt := c.sandboxAt
		c.mu.Unlock()
		ch := c.flight.DoChan("sandbox", func() (any, error) {
			if tok := c.sandboxToken(); tok != "" {
				return tok, nil
			}
			return c.prepareSandboxUnshared(ctx, projectID, entryAt)
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				return "", r.Err
			}
			tok, _ := r.Val.(string)
			return tok, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
}

// prepareSandboxUnshared 是预热链的真实实现（flight 内只跑一次）。
// entryAt 是入 flight 前读到的 sandboxAt：发布走 publishSandbox 做代际保护，
// 较慢的 flight 不得覆盖另一条 flight 已发布的更新令牌。
func (c *httpClient) prepareSandboxUnshared(ctx context.Context, projectID string, entryAt time.Time) (string, error) {
	tok, sandboxBase, err := c.runSandboxChain(ctx, projectID)
	if err != nil {
		return "", err
	}
	return c.publishSandbox(tok, sandboxBase, entryAt), nil
}

// runSandboxChain 只跑链拿令牌，不碰 c.sandbox/c.sandboxAt（发布统一走 publishSandbox）。
func (c *httpClient) runSandboxChain(ctx context.Context, projectID string) (tok, sandboxBase string, err error) {
	resp, err := c.requestRetry(ctx, http.MethodPost, c.origin()+PathBackendNew, map[string]any{}, nil, 3)
	if err != nil {
		return "", "", fmt.Errorf("prism: new backend: %w", err)
	}
	if resp.status >= 300 {
		return "", "", &upstreamError{Status: resp.status, Msg: "backend/1/new: " + truncate(string(resp.body), 200)}
	}
	v := resp.json()
	tok, _ = v["token"].(string)
	if tok == "" {
		return "", "", fmt.Errorf("prism: backend/1/new returned no sandbox token")
	}
	sandboxBase = strings.TrimRight(c.origin(), "/") + PathSandboxBase
	if u, _ := v["url"].(string); strings.TrimSpace(u) != "" {
		sandboxBase = strings.TrimRight(strings.TrimSpace(u), "/")
	}

	// 资源令牌：项目级 → 沙箱级。
	resp, err = c.requestRetry(ctx, http.MethodPost,
		fmt.Sprintf(c.origin()+PathResourcesToken, projectID),
		map[string]any{"sandbox_session_id": nil, "sandbox_token": tok}, nil, 3)
	if err != nil {
		return "", "", fmt.Errorf("prism: sandbox resources-token: %w", err)
	}
	if resp.status == http.StatusNotFound {
		// 项目被上游清理（2026-09-21 实锤：12h 风控期后旧项目全失效，
		// 404 "Invalid URL" 实为项目不存在）：清缓存重建项目后重试一次。
		log.Printf("prism: project %s gone (404), recreating", truncate(projectID, 12))
		c.setProject("")
		newPID, perr := c.ensureProject(ctx)
		if perr != nil {
			return "", "", fmt.Errorf("prism: recreate project after 404: %w", perr)
		}
		resp, err = c.requestRetry(ctx, http.MethodPost,
			fmt.Sprintf(c.origin()+PathResourcesToken, newPID),
			map[string]any{"sandbox_session_id": nil, "sandbox_token": tok}, nil, 3)
		if err != nil {
			return "", "", fmt.Errorf("prism: sandbox resources-token(retry): %w", err)
		}
		projectID = newPID
	}
	if resp.status >= 300 {
		return "", "", &upstreamError{Status: resp.status, Msg: "sandbox/resources-token: " + truncate(string(resp.body), 200)}
	}
	resourceToken, _ := resp.json()["access_token"].(string)

	resp, err = c.requestRetry(ctx, http.MethodPost, sandboxBase+PathProxyResources+"?prism_cache_bust="+cacheBust(),
		map[string]any{
			"token":           resourceToken,
			"resourceBaseUrl": strings.TrimRight(c.origin(), "/") + PathResourceBase,
			"projectId":       projectID,
		}, map[string]string{"X-Crixet-Sandbox-Token": tok}, 3)
	if err != nil {
		return "", "", fmt.Errorf("prism: register resource token: %w", err)
	}
	if resp.status >= 300 {
		return "", "", &upstreamError{Status: resp.status, Msg: "proxy/resources-token: " + truncate(string(resp.body), 200)}
	}

	// Y-Sweet（协作文件）令牌交棒：缺这一步 wait-for-sync 永远停在 syncing。
	resp, err = c.requestRetry(ctx, http.MethodPost, c.origin()+PathYSweet, map[string]any{"docId": projectID}, nil, 3)
	if err != nil {
		return "", "", fmt.Errorf("prism: fetch y-sweet token: %w", err)
	}
	if resp.status >= 300 {
		return "", "", &upstreamError{Status: resp.status, Msg: "api/y: " + truncate(string(resp.body), 200)}
	}
	y := resp.json()
	resp, err = c.requestRetry(ctx, http.MethodPost, sandboxBase+PathProxyToken+"?prism_cache_bust="+cacheBust(),
		map[string]any{
			"url":           y["url"],
			"baseUrl":       y["baseUrl"],
			"docId":         projectID,
			"token":         y["token"],
			"authorization": y["authorization"],
		}, map[string]string{"X-Crixet-Sandbox-Token": tok}, 3)
	if err != nil {
		return "", "", fmt.Errorf("prism: hand off y-sweet token: %w", err)
	}
	if resp.status >= 300 {
		return "", "", &upstreamError{Status: resp.status, Msg: "proxy/token: " + truncate(string(resp.body), 200)}
	}

	if err := c.waitForSync(ctx, sandboxBase, tok); err != nil {
		return "", "", err
	}
	return tok, sandboxBase, nil
}

// publishSandbox 发布链条拿到的令牌：只有当 c.sandboxAt 不比入 flight 前的 entryAt
// 新（或相等）才写入。另一条 flight 先发布了更新令牌时，丢弃自己的旧令牌并返回
// 当前生效的那个——调用方拿到的永远是最新发布，不会把旧令牌写回给上游用。
// now 由调用方注入（生产传 time.Now，测试传假时钟做确定性断言）。
func (c *httpClient) publishSandbox(tok, sandboxBase string, entryAt time.Time) string {
	return c.publishSandboxAt(tok, sandboxBase, entryAt, time.Now())
}

// publishSandboxAt 是 publishSandbox 的可测形态（时间可注入）。
func (c *httpClient) publishSandboxAt(tok, sandboxBase string, entryAt, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sandboxAt.After(entryAt) {
		// 另一条链路已发布更新的令牌：丢弃自己的旧令牌。
		return c.sandbox
	}
	c.sandbox = tok
	c.sandboxURL = sandboxBase
	c.sandboxAt = now
	return tok
}

// waitForSync 轮询到 status=synced（站点同款行为）。
func (c *httpClient) waitForSync(ctx context.Context, sandboxBase, tok string) error {
	last := ""
	for i := 0; i < syncPollTries; i++ {
		resp, err := c.requestRetry(ctx, http.MethodGet, sandboxBase+PathWaitForSync, nil,
			map[string]string{"X-Crixet-Sandbox-Token": tok}, 2)
		if err != nil {
			return fmt.Errorf("prism: wait-for-sync: %w", err)
		}
		if resp.status >= 300 {
			return &upstreamError{Status: resp.status, Msg: "wait-for-sync: " + truncate(string(resp.body), 200)}
		}
		last, _ = resp.json()["status"].(string)
		if last == "synced" {
			return nil
		}
	}
	return fmt.Errorf("prism: sandbox not synced (last status %q)", last)
}

type startResult struct {
	RequestID string
	ConvID    string
	TurnState json.RawMessage
	// T0.1：跨尝试累计的阶段耗时（ms）与 start 连接复用标记，Stream 收尾汇总用。
	SandboxMs   int64
	RegisterMs  int64
	StartMs     int64
	StartReused bool
	// P1-3 观测前置：本轮 start 上游调用次数（每次 start HTTP 尝试都计数，
	// 含换沙箱重试与 reconnect 同一沙箱重试），Stream 汇总成 upstream_calls。
	StartCalls int
}

// startTurn 发起一轮生成。沙箱没预热好时 start 会挂死，所以按超时重试一次。
func (c *httpClient) startTurn(ctx context.Context, nr *adapter.NativeRequest, projectID string) (*startResult, error) {
	// 上游会整段抽风（"Project conversation lookup failed (503)" / "Please submit prompt again"），
	// 每次换沙箱 + 换会话重来；重试到上限才把错误透给内核（否则账号会被冷却 2 分钟）。
	const attempts = 3
	var lastErr error
	// 项目归一（2026-09-21）：上游清理了 API 自建项目（自建→resources-token 404，
	// 重建后再 404），只认浏览器会话项目——沙箱链全程改用材料包的项目 ID，
	// 与 start body 的 metadata 自洽。
	if matMeta, _, _ := pickChatMaterial(ctx); matMeta != nil {
		if pid := strAt(matMeta, "projectId"); pid != "" && pid != projectID {
			log.Printf("prism: project switched to material project (acct=%s old=%s)", truncate(c.accountID(), 18), truncate(projectID, 8))
			projectID = pid
		}
	}
	// T0.1：跨尝试累计各阶段耗时，由 Stream 收尾汇总。
	var msSandbox, msRegister, msStart int64
	// P1-3 观测前置：本轮 start 上游调用次数（每次 start HTTP 尝试都计数）。
	var startCalls int
	// 连续传输层失败计数（超时/断连）：上游挂死风暴里沙箱多半是无辜的，
	// 首犯不作废沙箱、原地抖动重试；连续两犯才作废重铸（2026-09-20 实锤：
	// start 全超时期间重铸全部成功，杀沙箱纯属给过载上游补刀）。
	transportFails := 0
	// reconnecting 重试同一沙箱的次数（见下面 start 内联失败分支）。
	reconnects := 0
	for attempt := range attempts {
		if attempt > 0 {
			sleepCtx(ctx, time.Duration(attempt)*1500*time.Millisecond)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		// 会话必须与沙箱同批：重试会换沙箱，旧会话在上游会变成孤儿，所以每尝试一个新 conv。
		conv := "cdx1_" + uuid4()
		// T1.2：register 不依赖沙箱。热路径（沙箱缓存命中）直接 fire-and-forget，
		// 不阻塞 start（验收 register=0ms）；冷路径与预热链并发，join 后再发 start（省 1 RTT）。
		// register 失败（含 401/403）只记日志：401/403 提前发现能力丢失是接受的（ensureSession 覆盖）。
		var sandbox string
		if tok := c.sandboxToken(); tok != "" {
			tSb := time.Now()
			sandbox = tok
			msSandbox += time.Since(tSb).Milliseconds()
			go func() {
				tReg := time.Now()
				if err := c.registerConversation(ctx, projectID, conv); err != nil {
					log.Printf("prism: register conv=%s register_ms=%d failed (best-effort): %v",
						conv, time.Since(tReg).Milliseconds(), err)
				}
			}()
		} else {
			type sandboxOut struct {
				tok string
				ms  int64
				err error
			}
			type registerOut struct {
				ms  int64
				err error
			}
			// T3.3：冷链（ensureSandbox）失败就地指数退避重试 ≤2 次（1s/2s），耗尽才透给 Stream 换号。
			// 换号对冷链零收益（新号同样付冷链），只放大全池预热风暴。失败分类不变：仍原样透出上游类错误。
			// 重试走 ensureSandbox → prepareSandbox，仍共享包内 singleflight（key "sandbox"），不破坏 T2.2 合并。
			// start 本体失败路径（invalidateSandbox + attempt 循环 + attempt*1500ms）保持原样；register 并行（T1.2）原样保留。
			const prepareRetries = 2
			var sb sandboxOut
			regCh := make(chan registerOut, 1)
			tReg := time.Now()
			go func() {
				err := c.registerConversation(ctx, projectID, conv)
				regCh <- registerOut{ms: time.Since(tReg).Milliseconds(), err: err}
			}()
			for pr := 0; ; pr++ {
				sbCh := make(chan sandboxOut, 1)
				tSb := time.Now()
				go func() {
					tok, err := c.ensureSandbox(ctx, projectID)
					sbCh <- sandboxOut{tok: tok, ms: time.Since(tSb).Milliseconds(), err: err}
				}()
				sb = <-sbCh
				msSandbox += sb.ms
				if sb.err == nil {
					break
				}
				if pr >= prepareRetries || ctx.Err() != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					reg := <-regCh
					msRegister += reg.ms
					if reg.err != nil {
						log.Printf("prism: register conv=%s register_ms=%d failed (best-effort): %v",
							conv, reg.ms, reg.err)
					}
					return nil, sb.err
				}
				backoff := time.Duration(pr+1) * time.Second // 1s、2s
				log.Printf("prism: prepare sandbox failed (retry %d/2) conv=%s backoff=%v: %v",
					pr+1, conv, backoff, sb.err)
				sleepCtx(ctx, backoff)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			reg := <-regCh
			msRegister += reg.ms
			sandbox = sb.tok
			if reg.err != nil {
				log.Printf("prism: register conv=%s register_ms=%d failed (best-effort): %v",
					conv, reg.ms, reg.err)
			}
		}
		// metadata 优先用对话材料包（上游把 start 与页面上下文强绑定，自建
		// 会话+预热沙箱的 metadata 一律 400；材料可跨会话复用，见 sentinel.go
		// ——整套材料自洽，只盖回本请求的 model/effort 语义）。材料池 5 槽轮转，
		// 上游按浏览器身份排队/限流，单身份扛全池会被挂死（2026-09-20 实锤）。
		// 材料不可用时回退自建（旧行为）。
		metadata := map[string]any{
			"projectId":        projectID,
			"userId":           c.accountID(),
			"model":            modelName(nr),
			"reasoning_effort": reasoningEffort(nr),
			"frontend_origin":  c.origin(),
			"sandbox_url":      c.origin() + "/s/sandboxes/proxy/",
			"sandbox_token":    sandbox,
			"codex_listen_snapshot": listenSnapshot(
				c.accountID(), projectID, conv, c.origin()+"/s/sandboxes/proxy/", sandbox),
		}
		matMeta, matCookie, matSlotRef := pickChatMaterial(ctx)
		if matMeta != nil {
			metadata = make(map[string]any, len(matMeta)+2)
			for k, v := range matMeta {
				metadata[k] = v
			}
			metadata["model"] = modelName(nr)
			metadata["reasoning_effort"] = reasoningEffort(nr)
		}
		body := map[string]any{
			"input":          buildInput(nr),
			"metadata":       metadata,
			"conversationId": conv,
		}
		// 上游是 Responses 形状的接口（input/output items、previousResponseId），tools 平铺在顶层。
		if defs := toolDefs(nr); len(defs) > 0 {
			body["tools"] = defs
		}

		attemptCtx, cancel := context.WithTimeout(ctx, startTimeout)
		log.Printf("prism: start model=%q effort=%q conv=%s items=%d shape=%s",
			modelName(nr), reasoningEffort(nr), conv,
			len(body["input"].([]any)), inputShape(body["input"].([]any)))
		// PRISM_LOG_INPUT=1 时把整段 input 打出来，用来核对「上游到底收到了什么提示词」。
		if os.Getenv("PRISM_LOG_INPUT") != "" {
			if b, err := json.Marshal(body["input"]); err == nil {
				log.Printf("prism: start input=%s", truncate(string(b), 4000))
			}
		}
		tStart := time.Now()
		startCalls++ // 每次 start 上游调用都计数（换沙箱重试与 reconnect 重试都算）。
		// 对话面 sentinel 风控（2026-09-19 起强制）：start 必须带一次性
		// openai-sentinel-token，缺失/复用一律应用层 403，且实测无 token 的
		// start 会被上游挂死到超时。token 严格一次性，放在 attempt 循环内
		// 每次真发前铸一个；铸造失败时跳过本次尝试重试（显式 off 才裸发）。
		var extraHeaders map[string]string
		if tok, terr := takeSentinelToken(ctx); terr == nil {
			extraHeaders = map[string]string{"openai-sentinel-token": tok}
		} else if !sentinelEnabled() {
			log.Printf("prism: start without sentinel token (disabled): %v", terr)
		} else {
			lastErr = fmt.Errorf("prism: sentinel token unavailable: %w", terr)
			log.Printf("prism: start attempt %d skipped (sentinel mint failed): %v", attempt+1, terr)
			cancel() // attemptCtx 已建，continue 前释放（vet：泄漏路径）
			continue
		}
		// 材料在场时连 cookie 一起换：上游校验的不止 sentinel，还有 cookie 指纹
		// （服务自拼 2-token cookie 实测 400，材料附带浏览器全套 cookie 可用）。
		// request() 的 headers 循环在默认 Cookie 之后执行，同名键覆盖生效。
		if matCookie != "" {
			if extraHeaders == nil {
				extraHeaders = map[string]string{}
			}
			extraHeaders["Cookie"] = matCookie
		}
		resp, err := c.request(attemptCtx, http.MethodPost, c.origin()+PathChat, body, extraHeaders)
		msStart += time.Since(tStart).Milliseconds()
		cancel()
		if err != nil {
			lastErr = err
			adapter.StormRecord(false)
			transportFails++
			log.Printf("prism: start attempt %d transport error (consecutive=%d, sandbox kept): %v", attempt+1, transportFails, err)
			if ctx.Err() != nil {
				return nil, err
			}
			if transportFails < 2 {
				sleepCtx(ctx, time.Duration(500+int(time.Now().UnixNano()%500))*time.Millisecond)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			c.invalidateSandbox()
			continue
		}
		transportFails = 0
		if resp.status >= 300 {
			lastErr = &upstreamError{Status: resp.status, Msg: "start: " + truncate(string(resp.body), 300)}
			if resp.status >= 500 {
				adapter.StormRecord(false)
				c.invalidateSandbox()
				continue
			}
			return nil, lastErr
		}
		v := resp.json()
		out := &startResult{
			RequestID:   stringAt(v, "request_id"),
			ConvID:      stringAt(v, "conversation_id"),
			TurnState:   rawAt(v, "turn_state"),
			SandboxMs:   msSandbox,
			RegisterMs:  msRegister,
			StartMs:     msStart,
			StartReused: resp.reused,
			StartCalls:  startCalls,
		}
		// 上游把 input 编成真正喂给模型的 prompt（含它对附件的展开，如 "[project file: …]"）。
		// PRISM_LOG_PROMPT=1 时打出来——这是"模型到底看到了什么"的唯一现场证据。
		if os.Getenv("PRISM_LOG_PROMPT") != "" {
			if ts := mapAt(v, "turn_state"); ts != nil {
				log.Printf("prism: turn prompt=%s", truncate(stringAt(ts, "prompt"), 6000))
			}
		}
		if out.RequestID != "" && len(out.TurnState) > 0 && out.TurnState[0] == '{' {
			adapter.StormRecord(true)
			return out, nil
		}
		// 上游会以 200 + status=completed + response.status=error 立刻返回失败
		//（例如 "Error while processing conversation (500 …). Please submit prompt again."），
		// 这种 start 没有 turn_state，直接轮询会被上游回 400。
		msg := inlineStartError(v)
		// 材料疑似失效（上游页面上下文过期/变化）→ 只作废本次用的槽位，下一轮刷新。
		if strings.Contains(msg, "400 Bad Request") && matSlotRef != nil {
			matSlotRef.invalidate()
		}
		// 沙箱刚建好、工作区还在 sync：等 codex 就绪后**重试同一个沙箱**。
		// 原来直接换沙箱重来，等于白付一次 6~18s 的预热（参考实现不做换沙箱）。
		if isSandboxReconnecting(v, msg) && reconnects < maxSandboxReconnects {
			reconnects++
			log.Printf("prism: sandbox reconnecting（第 %d/%d 次）→ 等 codex ready 后重试同一沙箱",
				reconnects, maxSandboxReconnects)
			c.waitCodexReady(ctx, sandbox)
			if ctx.Err() == nil {
				sleepCtx(ctx, sandboxReconnectWait)
			}
			// 上游的 for attempt := range attempts 每轮都会重置 attempt（range int 语义），
			// 所以重试同一沙箱会消耗一次 attempt —— 总数仍由 attempts × maxSandboxReconnects 双重有界。
			continue
		}
		se := startFailedError(msg)
		lastErr = se
		log.Printf("prism: start attempt %d unusable model=%q effort=%q conn_reused=%t (status=%q body=%s)",
			attempt+1, modelName(nr), reasoningEffort(nr), resp.reused, stringAt(v, "status"), truncate(string(resp.body), 800))
		if !se.retryable() {
			// 请求过错（模型名不在账号清单里、上下文超长）：换沙箱重试没有任何意义，只会白等。
			adapter.StormRecord(false)
			return nil, se
		}
		adapter.StormRecord(false)
		c.invalidateSandbox()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("prism: start failed")
	}
	return nil, lastErr
}

// inlineStartError 抽 start 内联失败的文案：response.payload.message，退到顶层 message。
func inlineStartError(v map[string]any) string {
	if resp := mapAt(v, "response"); resp != nil {
		if pay := mapAt(resp, "payload"); pay != nil {
			if msg := stringAt(pay, "message"); msg != "" {
				return msg
			}
		}
		if msg := stringAt(resp, "message"); msg != "" {
			return msg
		}
	}
	if msg := stringAt(v, "message"); msg != "" {
		return msg
	}
	return "unknown upstream error"
}

type pollResult struct {
	Done      bool
	Text      string
	Reasoning string // 上游 reasoning item 的 summary_text（思考摘要），空=本轮无思考
	Err       string
	ToolCalls []adapter.StreamedToolCall
	ItemTypes string
	// T0.1：Stream 汇总用（总 status 调用次数与轮询总耗时 ms）。
	Polls  int
	PollMs int64
}

// outputItemTypes 汇总 payload.output 里出现过的 item 类型（诊断上游是否回话工具调用）。
// inputShape 汇总 input 数组的形状：每项的 role 与文本长度，例如 "system(34)/user(52)"；
// 带附件时写成 "user(52+1f)"（f = input_file 块数）。
// 上游只把最后一条 user 当本轮请求，其余 system 项是上下文——这条日志是「谁被送上去」的唯一现场证据。
func inputShape(items []any) string {
	parts := make([]string, 0, len(items))
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok {
			parts = append(parts, "?")
			continue
		}
		role := stringAt(it, "role")
		if role == "" {
			role = stringAt(it, "type")
		}
		n, files := 0, 0
		if arr, ok := it["content"].([]any); ok {
			for _, c := range arr {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if stringAt(cm, "type") == "input_file" {
					files++
					continue
				}
				n += len(stringAt(cm, "text"))
			}
		}
		if files > 0 {
			parts = append(parts, fmt.Sprintf("%s(%d+%df)", role, n, files))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%d)", role, n))
	}
	return strings.Join(parts, "/")
}

func outputItemTypes(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	outs, _ := payload["output"].([]any)
	if len(outs) == 0 {
		return ""
	}
	counts := make([]string, 0, len(outs))
	seen := map[string]int{}
	for _, it := range outs {
		m, _ := it.(map[string]any)
		t := stringAt(m, "type")
		if t == "" {
			t = "?"
		}
		seen[t]++
	}
	for t, n := range seen {
		counts = append(counts, fmt.Sprintf("%s x%d", t, n))
	}
	sort.Strings(counts)
	return strings.Join(counts, ",")
}

// toolDefs 把内核的 OpenAI function 工具编成上游 Responses 形状（平铺，不套 function 层）。
func toolDefs(nr *adapter.NativeRequest) []any {
	out := make([]any, 0, len(nr.Tools))
	for _, t := range nr.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		def := map[string]any{"type": "function", "name": name}
		if d := strings.TrimSpace(t.Description); d != "" {
			def["description"] = d
		}
		if len(t.Parameters) > 0 {
			var schema any
			if err := json.Unmarshal(t.Parameters, &schema); err == nil {
				def["parameters"] = schema
			}
		}
		out = append(out, def)
	}
	return out
}

// extractToolCalls 取 output 里的 function_call 项。
// 上游一旦执行了工具（如 Codex 的 shell/file 工具）也会出现在这里，这里只做搬运。
func extractToolCalls(payload map[string]any) []adapter.StreamedToolCall {
	if payload == nil {
		return nil
	}
	outs, _ := payload["output"].([]any)
	var calls []adapter.StreamedToolCall
	for _, it := range outs {
		m, _ := it.(map[string]any)
		kind := stringAt(m, "type")
		switch kind {
		case "function_call", "custom_tool_call", "tool_call":
		default:
			continue
		}
		name := stringAt(m, "name")
		if name == "" {
			continue
		}
		id := stringAt(m, "call_id")
		if id == "" {
			id = stringAt(m, "id")
		}
		args := strings.TrimSpace(stringAt(m, "arguments"))
		if args == "" {
			args = "{}"
		}
		calls = append(calls, adapter.StreamedToolCall{
			Tool:       name,
			Name:       name,
			ToolCallID: id,
			RawArgs:    args,
			ToolIndex:  uint32(len(calls)),
			Kind:       kind,
		})
	}
	return calls
}

// pollInterval 是第 n 次 status 调用后的等待时长（T5.1 终局）：前 3 次急 poll
// 100ms（实测快速 pending 后服务端长保持，sleep 纯属浪费），之后 0.4s 起步
// +0.4s 到 2s 封顶。序列：0.1/0.1/0.1/0.4/0.8/1.2/1.6/2.0/2.0…
func pollInterval(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n <= 3 {
		return 100 * time.Millisecond
	}
	d := time.Duration(n-3) * 400 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// pollTurnBudget 单轮生成的轮询总预算：客户端不断连时 ctx 不会取消，大 prompt 遇上游
// 劣化会拖满数分钟（真机见过 4m36s）白占并发；超预算快速失败（504，可换号重试）。
// PRISM_TURN_BUDGET_SECONDS 可调，<=0 或非法回默认。
const pollTurnBudget = 240 * time.Second

func turnBudget() time.Duration {
	if v := strings.TrimSpace(os.Getenv("PRISM_TURN_BUDGET_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return pollTurnBudget
}

// pollTurn 轮询到 completed/失败。turn_state 必须逐轮回填（真机验证）。
// 每次循环都打一次 status 上游调用，attempt 即 poll 上游调用次数（Polls），
// Stream 汇总成 upstream_calls 的一部分（start_calls+poll_calls）。
func (c *httpClient) pollTurn(ctx context.Context, st *startResult, emit func(adapter.Event) bool) (*pollResult, error) {
	tAll := time.Now()
	deadline := tAll.Add(turnBudget())
	attempt := 0
	failures := 0
	for {
		attempt++
		body := map[string]any{"request_id": st.RequestID}
		if len(st.TurnState) > 0 {
			body["turn_state"] = st.TurnState
		}
		callCtx, cancel := context.WithTimeout(ctx, pollCallTimeout)
		tCall := time.Now()
		resp, err := c.request(callCtx, http.MethodPost, c.origin()+PathStatus, body, nil)
		holdMs := time.Since(tCall).Milliseconds()
		cancel()
		if err != nil {
			log.Printf("prism: poll attempt=%d hold_ms=%d status=error err=%s", attempt, holdMs, truncate(err.Error(), 200))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures++
			if failures > 10 {
				return nil, fmt.Errorf("prism: status polling failed: %w", err)
			}
			sleepCtx(ctx, pollInterval(attempt))
			continue
		}
		if resp.status >= 500 {
			// 抓包里出现过 503 upstream connect error：可重试。
			log.Printf("prism: poll attempt=%d hold_ms=%d status=%d conn_reused=%t", attempt, holdMs, resp.status, resp.reused)
			failures++
			if failures > 10 {
				return nil, &upstreamError{Status: resp.status, Msg: "status: " + truncate(string(resp.body), 200)}
			}
			sleepCtx(ctx, pollInterval(attempt))
			continue
		}
		if resp.status >= 300 {
			log.Printf("prism: poll attempt=%d hold_ms=%d status=%d conn_reused=%t", attempt, holdMs, resp.status, resp.reused)
			return nil, &upstreamError{Status: resp.status, Msg: "status: " + truncate(string(resp.body), 200)}
		}
		failures = 0
		v := resp.json()
		if ts := rawAt(v, "turn_state"); len(ts) > 0 && string(ts) != "null" {
			st.TurnState = ts
		}
		status := stringAt(v, "status")
		log.Printf("prism: poll attempt=%d hold_ms=%d status=%q conn_reused=%t", attempt, holdMs, status, resp.reused)
		switch status {
		case "completed":
			// PRISM_LOG_PAYLOAD=1 时打出 completed 原始响应：核对上游到底回了哪些 item
			// （reasoning/thinking 是否在 payload 里——决定能否把思考链透传给客户端并计入 completion）。
			if os.Getenv("PRISM_LOG_PAYLOAD") != "" {
				log.Printf("prism: completed payload=%s", truncate(string(resp.body), 6000))
			}
			payload := mapAt(mapAt(v, "response"), "payload")
			return &pollResult{
				Done:      true,
				Text:      extractText(payload),
				Reasoning: extractReasoning(payload),
				ToolCalls: extractToolCalls(payload),
				ItemTypes: outputItemTypes(payload),
				Polls:     attempt,
				PollMs:    time.Since(tAll).Milliseconds(),
			}, nil
		case "failed", "error":
			return &pollResult{Err: truncate(string(resp.body), 300), Polls: attempt, PollMs: time.Since(tAll).Milliseconds()}, nil
		case "pending", "started", "":
			// 继续轮询
		default:
			// 未知状态按 pending 处理，靠 ctx 兜底。
		}
		if time.Now().After(deadline) {
			log.Printf("prism: poll budget exceeded attempt=%d budget=%s request_id=%s", attempt, turnBudget(), st.RequestID)
			return nil, &upstreamError{Status: http.StatusGatewayTimeout, Msg: fmt.Sprintf("turn budget %s exceeded", turnBudget())}
		}
		sleepCtx(ctx, pollInterval(attempt))
	}
}

// extractText 从 completed payload 里取出正文（output[].content[].text）。
func extractText(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	var sb strings.Builder
	outs, _ := payload["output"].([]any)
	for _, it := range outs {
		m, _ := it.(map[string]any)
		if t, _ := m["type"].(string); t != "" && t != "message" {
			continue
		}
		contents, _ := m["content"].([]any)
		for _, cc := range contents {
			cm, _ := cc.(map[string]any)
			switch stringAt(cm, "type") {
			case "output_text", "text":
				sb.WriteString(stringAt(cm, "text"))
			}
		}
	}
	return sb.String()
}

// extractReasoning 从 completed payload 里取出思考摘要（output[] 中 type=="reasoning" 项的
// summary[].text）。上游实测结构：{"type":"reasoning","summary":[{"type":"summary_text","text":...}]}。
// 多段 summary 用换行拼接；无 reasoning 项返回空串。
func extractReasoning(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	var sb strings.Builder
	outs, _ := payload["output"].([]any)
	for _, it := range outs {
		m, _ := it.(map[string]any)
		if stringAt(m, "type") != "reasoning" {
			continue
		}
		summaries, _ := m["summary"].([]any)
		for _, s := range summaries {
			sm, _ := s.(map[string]any)
			if stringAt(sm, "type") != "summary_text" {
				continue
			}
			if t := stringAt(sm, "text"); t != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func cacheBust() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d", time.Now().UnixNano()/1e6) + hex.EncodeToString(b)[:6]
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func stringAt(v map[string]any, key string) string {
	if v == nil {
		return ""
	}
	s, _ := v[key].(string)
	return s
}

func mapAt(v map[string]any, key string) map[string]any {
	if v == nil {
		return nil
	}
	m, _ := v[key].(map[string]any)
	return m
}

func rawAt(v map[string]any, key string) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v[key])
	if err != nil || string(raw) == "null" {
		return nil
	}
	return raw
}
