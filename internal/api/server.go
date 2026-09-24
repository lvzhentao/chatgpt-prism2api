package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/admin"
	"prism-2api/internal/auth"
	"prism-2api/internal/clientkeys"
	"prism-2api/internal/config"
	"prism-2api/internal/debugtrace"
	"prism-2api/internal/emulation"
	"prism-2api/internal/failclass"
	"prism-2api/internal/groups"
	"prism-2api/internal/persist"
	"prism-2api/internal/pg"
	"prism-2api/internal/pool"
	"prism-2api/internal/proxypool"
	"prism-2api/internal/scheduler"
	"prism-2api/internal/secret"
	"prism-2api/internal/tasks"
)

// Server 是 OpenAI 兼容 API 服务。
type Server struct {
	cfg           *config.Config
	runtime       *admin.RuntimeConfig // 运行时可变配置（Web 端可改）
	logs          *admin.LogStore      // 请求日志
	meters        *meters              // M1 实时 RPM/TPM 计量（access 中间件打点）
	gateMu        sync.Mutex
	gate          chan struct{} // M2 入口并发闸门（0 容量=直通）
	gateCheckedAt time.Time     // 闸门容量上次重算时间
	catalog       adapter.Catalog
	pool          *pool.Pool
	sched         *scheduler.Scheduler
	mock          *adapter.MockBackend
	sess          *admin.SessionManager // 管理端登录会话
	started       time.Time
	ka            *auth.Keepalive // 后台 Token 保活

	keys    *clientkeys.Store // W2.2 入口 Key
	groups  *groups.Store     // W2.3 分组；未加载时管理 JSON 返回 503
	proxies *proxypool.Store
	jobs    *tasks.Runner

	batchStore *batchStore      // Message Batches API
	pg         *pg.DB           // PostgreSQL：全部持久状态
	emu        *emulation.Store // 协议外观：缓存 / web_search / 签名
}

// NewServer 创建 API 服务。
func NewServer(cfg *config.Config) (*Server, error) {
	s := &Server{cfg: cfg, started: time.Now(), meters: newMeters()}

	if cfg != nil {
		secret.Configure(cfg.EncryptKey)
	} else {
		secret.ConfigureFromEnv()
	}

	backend, err := s.openStore(cfg)
	if err != nil {
		return nil, err
	}

	rc, err := admin.Open(backend, cfg.APIBaseURL, cfg.WebsiteURL, cfg.APIKeyAuth, cfg.ListenAddr, cfg.MockMode)
	if err != nil {
		return nil, fmt.Errorf("load runtime config: %w", err)
	}
	s.runtime = rc
	s.logs = admin.NewLogStore(rc.LogMax)
	if s.pg != nil {
		s.logs.SetSink(s.pg)
		go s.purgeLogsLoop()
	}
	s.sess = admin.NewSessionManager()
	s.batchStore = newBatchStore()

	store, err := clientkeys.Open(backend)
	if err != nil {
		return nil, fmt.Errorf("load client keys: %w", err)
	}
	s.keys = store
	s.ensureSystemKey()
	if rc.MustChange() {
		log.Printf("WARNING: default admin credentials (admin/admin123) are in use; must_change_password is set. Admin APIs except login/logout/password are blocked until the password is changed.")
	}

	p, err := pool.Open(backend, cfg.APIBaseURL, cfg.WebsiteURL, cfg.ClientVersion, cfg.ClientType)
	if err != nil {
		return nil, fmt.Errorf("init account pool: %w", err)
	}
	s.pool = p
	s.sched = scheduler.New(schedulerFromRuntime(rc))

	gs, err := groups.Open(backend)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	gs.SetAccountRenamer(p)
	if s.keys != nil {
		gs.SetKeyRenamer(keyGroupRenamer{s.keys})
	}
	s.groups = gs
	s.sched.SetLimitResolver(func(names []string) (rpm, dailyMax int) {
		for _, n := range names {
			r, d := gs.Limit(n)
			if rpm == 0 {
				rpm = r
			}
			if dailyMax == 0 {
				dailyMax = d
			}
		}
		return
	})
	// 单号并发上限/429 降级值：闭包实时读 RuntimeConfig（管理端热改即时生效）。
	p.SetConcurrencyResolver(func() (base, degraded int) {
		return rc.AccountConcurrencyN(), rc.AccountConcurrency429N()
	})

	px, err := proxypool.Open(backend)
	if err != nil {
		return nil, fmt.Errorf("load proxies: %w", err)
	}
	s.proxies = px
	emu, err := emulation.Open(backend)
	if err != nil {
		return nil, fmt.Errorf("load emulation: %w", err)
	}
	s.emu = emu
	jobStore, err := tasks.Open(backend)
	if err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}
	s.jobs = tasks.NewRunner(jobStore, rc.BatchConcurrencyN())
	s.pool.SetEgressResolver(s.resolveAccountEgress)

	// 启动参数中的 API Key 入池（--api-key → default；--api-keys → api-key-N）
	if cfg.APIKey != "" {
		if err := p.AddAPIKey("default", cfg.APIKey); err != nil {
			return nil, err
		}
	}
	for i, k := range cfg.APIKeys {
		name := fmt.Sprintf("api-key-%d", i+1)
		if err := p.AddAPIKey(name, k); err != nil {
			return nil, err
		}
	}

	s.catalog = adapter.NewCatalog()
	s.catalog.SetClientProvider(s.poolClient)

	if cfg.MockMode {
		s.mock = adapter.NewMockBackend(cfg.MockAddr)
		if err := s.mock.Start(); err != nil {
			return nil, fmt.Errorf("start mock backend: %w", err)
		}
		// mock 模式下将池指向 mock 后端
		s.pool.SetBaseURL("http://" + cfg.MockAddr)
		log.Printf("mock backend listening on %s", cfg.MockAddr)
	}
	s.batchStore.setPersist(s.pg)
	s.restoreBatches()
	s.applyRuntimeSideEffects()
	s.startKeepalive()
	s.startPrewarm()
	return s, nil
}

// Shutdown 优雅关闭：停预热/保活循环后刷脏（pool 账号 + clientkeys 用量）。
// T3.1/T3.2 的异步落盘在此收敛，崩溃只丢最后一个合并窗口（pool ≤1s、keys ≤100条）。
func (s *Server) Shutdown() {
	if s == nil {
		return
	}
	s.stopPrewarm()
	if s.ka != nil {
		s.ka.Stop()
	}
	if s.pool != nil {
		s.pool.Flush()
	}
	if s.keys != nil {
		s.keys.Flush()
	}
}

func (s *Server) openStore(cfg *config.Config) (persist.Backend, error) {
	if cfg == nil {
		return persist.NewMemory(), nil
	}
	if cfg.DatabaseURL != "" {
		db, err := pg.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		s.pg = db
		imported, err := pg.ImportLegacy(context.Background(), db, cfg.CredentialDir)
		if err != nil {
			db.Close()
			s.pg = nil
			return nil, fmt.Errorf("import legacy json: %w", err)
		}
		if imported {
			log.Printf("imported legacy JSON into postgres (files are no longer written)")
		}
		log.Printf("postgres connected (all durable state)")
		return db, nil
	}
	if cfg.MockMode {
		backend := persist.NewMemory()
		if _, err := persist.ImportLegacyDir(backend, cfg.CredentialDir); err != nil {
			return nil, fmt.Errorf("import legacy json: %w", err)
		}
		log.Printf("mock mode without DATABASE_URL: in-memory store only")
		return backend, nil
	}
	return nil, fmt.Errorf("DATABASE_URL / WEB2API_DATABASE_URL is required; PostgreSQL is the only durable store")
}

// poolClient 从账号池选一个可用账号的客户端（模型加载用）。
func (s *Server) poolClient() adapter.Client {
	acc, err := s.pickAny()
	if err != nil {
		return nil
	}
	return acc.Client
}

func (s *Server) pickAny() (*pool.Account, error) {
	if s.sched != nil {
		return s.sched.Pick(s.pool.Accounts(), scheduler.Request{})
	}
	return s.pool.Next()
}

// Handler 返回 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// OpenAI 兼容端点
	mux.HandleFunc("/v1/models", s.requireAPIKey(s.handleModels))
	mux.HandleFunc("/v1/models/refresh", s.handleModelsRefresh)
	mux.HandleFunc("/v1/chat/completions", s.requireAPIKey(s.handleChat))
	// OpenAI Responses API（官方 /v1/responses）
	mux.HandleFunc("/v1/responses", s.requireAPIKey(s.handleResponses))
	// Google Gemini API（官方 /v1beta 与 /v1 路径）
	mux.HandleFunc("/v1beta/models", s.requireAPIKey(s.handleGeminiModels))
	mux.HandleFunc("/v1beta/models/", s.requireAPIKey(s.handleGemini))
	mux.HandleFunc("/v1/models/", s.requireAPIKey(s.handleGemini))

	// Anthropic 兼容端点 (Claude Code / Composer)
	mux.HandleFunc("/v1/messages", s.requireAPIKey(s.handleAnthropicMessages))
	mux.HandleFunc("/messages", s.requireAPIKey(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	mux.HandleFunc("/messages/count_tokens", s.handleAnthropicCountTokens)
	// Message Batches API（对齐官方）
	mux.HandleFunc("/v1/messages/batches", s.requireAPIKey(s.handleBatchesCreateList))
	mux.HandleFunc("/v1/messages/batches/", s.requireAPIKey(s.handleBatchesSubresource))

	// 登录与账号池
	mux.HandleFunc("/v1/login", s.handleLogin)
	mux.HandleFunc("/v1/auth/status", s.handleAuthStatus)
	mux.HandleFunc("/v1/auth/api-key", s.handleAPIKey)
	mux.HandleFunc("/v1/accounts", s.handleAccounts)
	mux.HandleFunc("/v1/accounts/", s.handleAccountDelete)
	// 管理 JSON（开发时由 Vite :5173 代理；生产由 mountAdminStatic 托管 /admin/）
	mux.HandleFunc("/api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("/api/admin/logout", s.handleAdminLogout)
	mux.HandleFunc("/api/admin/password", s.adminAuth(s.handleAdminPassword))
	mux.HandleFunc("/api/admin/config", s.adminAuth(s.handleAdminConfig))
	mux.HandleFunc("GET /api/admin/config/schema", s.adminAuth(handleAdminConfigSchema))
	mux.HandleFunc("/api/admin/logs", s.adminAuth(s.handleAdminLogs))
	mux.HandleFunc("GET /api/admin/logs/{id}", s.adminAuth(s.handleAdminLogGet))
	mux.HandleFunc("/api/admin/stats", s.adminAuth(s.handleAdminStats))
	mux.HandleFunc("/api/admin/accounts", s.adminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.pg != nil {
			if err := s.pg.Ping(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_down"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mountAdminJSON(mux)
	s.mountPprof(mux)
	s.mountAdminStatic(mux)
	return s.logMiddleware(mux)
}

// requireAPIKey 公开 API 鉴权（对齐 kiro auth_middleware）：
//  1. 先走 ClientKeyManager（含 ensure_system_key 注册的 master）；
//  2. store 为 nil 时才回退常量时间比对 runtime api_key_auth；
//  3. runtime api_key_auth 为空 = 不鉴权（本地/mock）。
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := extractAPIKey(r)
		if s.keys != nil && presented != "" {
			if rec, ok := s.keys.VerifyAndTouch(presented); ok {
				if e := logEntryFrom(r); e != nil {
					id := rec.ID
					e.ClientKeyID = &id
				}
				next(w, r.WithContext(WithIdentity(r.Context(), Identity{KeyID: rec.ID, Group: rec.Group})))
				return
			}
		}
		runtimeKey := s.runtimePublicKey()
		if runtimeKey == "" {
			next(w, r)
			return
		}
		if s.keys == nil && presented != "" && clientkeys.Equal(presented, runtimeKey) {
			next(w, r.WithContext(WithIdentity(r.Context(), Identity{KeyID: 0})))
			return
		}
		s.writeAPIKeyUnauthorized(w, r)
	}
}

func (s *Server) runtimePublicKey() string {
	if s.runtime != nil {
		return s.runtime.PublicAPIKey()
	}
	if s.cfg != nil {
		return s.cfg.APIKeyAuth
	}
	return ""
}

func (s *Server) ensureSystemKey() {
	if s.keys == nil {
		return
	}
	if k := s.runtimePublicKey(); k != "" {
		s.keys.EnsureSystemKey("系统密钥", "bootstrap from api_key_auth", k)
	}
}

func (s *Server) writeAPIKeyUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.HasPrefix(r.URL.Path, "/messages") {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Incorrect API key provided")
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": map[string]any{
			"message": "Incorrect API key provided",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "invalid_api_key",
		},
	})
}

// handleModels 暴露裸名 + 思维/档位变体。裸名请求按客户端思考参数解析到变体。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ids := s.catalog.PublicModelIDs()
	if ids == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": "load models failed", "type": "upstream_error"},
		})
		return
	}
	models := make([]ModelObject, 0, len(ids))
	for _, id := range ids {
		models = append(models, ModelObject{
			ID: id, Object: "model", Created: time.Now().Unix(), OwnedBy: adapter.Name(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// handleModelsRefresh 强制刷新模型缓存（POST），返回最新模型列表。
// 刷新失败时保留旧缓存并返回上游错误。
func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]string{"message": "method not allowed, use POST", "type": "invalid_request_error"},
		})
		return
	}
	start := time.Now()
	if _, err := s.catalog.Refresh(); err != nil {
		log.Printf("models refresh failed: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": "refresh models: " + err.Error(), "type": "upstream_error"},
		})
		return
	}
	ids := s.catalog.PublicModelIDs()
	models := make([]ModelObject, 0, len(ids))
	for _, id := range ids {
		models = append(models, ModelObject{
			ID: id, Object: "model", Created: time.Now().Unix(), OwnedBy: adapter.Name(),
		})
	}
	log.Printf("models refreshed: %d models (%s)", len(models), time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// handleChat 处理对话（流式与非流式）。
// applyOpenAIThinkingDefaults 处理 OpenAI 入口的思考/输出参数：
//  1. max_completion_tokens 兼容（omp 等客户端用它替代 max_tokens）
//  2. 模型名带思考/等级变体（claude-opus-4-8-thinking-high / -high / -max / -low）
//     时补全 reasoning_effort 并还原裸模型名，让 thinking 参数随变体正确传递——
//     否则选 -thinking-* 变体也不注入思考参数，模型表现为"不思考"。
func applyOpenAIThinkingDefaults(req *ChatCompletionRequest) {
	if req.MaxCompletionTokens != nil && req.MaxTokens == nil {
		mt := *req.MaxCompletionTokens
		req.MaxTokens = &mt
	}
	if req.ReasoningEffort != "" {
		return
	}
	base, lvl, _ := adapter.ParseModelID(req.Model)
	if lvl == "" || base == req.Model {
		return
	}
	req.Model = base
	switch lvl {
	case "medium":
		req.ReasoningEffort = "medium"
	case "low":
		req.ReasoningEffort = "low"
	case "xhigh":
		// 实验档透传（P2-1 放开侧）：-xhigh/-max 不再被这层夹成 high，
		// adapter 的 reasoningEffort 会原样发上游。
		req.ReasoningEffort = "xhigh"
	default:
		req.ReasoningEffort = "high"
	}
	// 注意：不应用 capEffort——用户显式选择 -thinking-high/-max 变体是明确意图，
	// 默认上限会把它静默压回 low，重新制造"选了高思考却不思考"的问题。
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	raw, err := readJSONBody(r, &req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "invalid request body: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "model is required", "type": "invalid_request_error"},
		})
		return
	}
	applyOpenAIThinkingDefaults(&req)
	req.Model = s.resolveModel(req.Model)

	agentReq, err := MapChat(r.Context(), &req, s.promptText())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	s.logPromptSource(agentReq)

	entry := logEntryFrom(r)
	if entry != nil {
		entry.Model = req.Model
		entry.Stream = req.Stream
	}
	iter := s.newAccountIter(r.Context(), r, req.Model, raw)
	// 模拟缓存：按原始请求体算前缀指纹（ Anthropic 入口同款，见 cachePlan）。
	// 只改客户端可见 usage，绝不动上游请求；成功才 Commit，失败不污染前缀表。
	cachePlan := s.cachePlan(r, raw, req.Model, estimateInputTokens(&req))
	if req.Stream {
		s.streamChat(w, r, &req, agentReq, entry, iter, cachePlan)
		return
	}
	s.nonStreamChat(w, r, &req, agentReq, entry, iter, cachePlan)
}

// nonStreamFinishReason 返回非流式对话的 finish_reason（工具调用时用 tool_calls）。
func nonStreamFinishReason(toolCalled bool) string {
	if toolCalled {
		return "tool_calls"
	}
	return "stop"
}

// maxTries 保留给旧循环；新路径用 accountIter + max-retry-credentials。
func (s *Server) maxTries() int {
	n := s.pool.Count()
	if s.sched != nil {
		cfg := s.sched.Config()
		if limit := cfg.CredentialLimit(); limit > 0 && (n == 0 || limit < n) {
			n = limit
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// emptyRetryMax 是空 completion（上游假完成：整轮零正文/思考/工具调用）的换号重试上限。
// 空完成不是账号故障（不打冷却、不计 failclass），只是这一轮换张号重碰；
// 2 次足够覆盖偶发，又不至于在上游系统性返空时把单个请求放大成扫池。
const emptyRetryMax = 2

// streamChat 流式对话：adapter.Event → OpenAI SSE。
// WriteHeader 后立即发出 role 首帧（首帧提前 TTFB 优化）；只有发出内容帧
// （Text/Thinking/ToolCall delta）后才禁止换号重试，连接/首字节阶段失败仍可静默换号。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *ChatCompletionRequest, agentReq *adapter.NativeRequest, entry *admin.LogEntry, iter *accountIter, cachePlan *emulation.Plan) {
	start := time.Now()
	if stormFastFail(w, r, "openai") {
		return
	}
	// 身份闸（只在身份话题触发，普通对话零开销）：gate 命中时先攒全文，
	// 结束时清洗完一次性发出；未命中走原来的逐帧透传。
	gateQuestion, gateOn := "", false
	if s.runtimeIdentityGuardOn() {
		gateQuestion, gateOn = identityGate(req)
	}

	tPick0 := time.Now()
	acc, err := iter.Next()
	log.Printf("prism: t_pick attempt=0 account=%q inflight=%d latency_ms=%d err=%v", accName(acc), accInflight(acc), time.Since(tPick0).Milliseconds(), err)
	if err != nil {
		writePickError(w, err, false)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ss := NewSSEWriter(w)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	role := "assistant"
	first := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Role: role}}}}
	sentFirst := false // role 首帧是否已发出；只发一次，换号重试不再重发（T1.0）
	// 空 completion 换号重试计数：上游「假完成」（整轮零产出）时换号重来，
	// 至多 emptyRetryMax 次，仍空则按上游错误收尾——空 200 在 NewAPI 面板是
	// 0 t/s 假成功（实测 24h 约 900 例），客户拿到空消息也无法重试。
	emptyRetries := 0
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// 重试总预算：上游抽风时换号重试链每轮可烧 60-75s，链到天荒地老只会
			// 把客户端吊到 NewAPI 渠道超时（180s/300s）变成 client_gone——不如在
			// 预算内返回干净的上游错误，客户可立即重试（线上 2026-09-20 实锤）。
			if b := retryBudget(); b > 0 && time.Since(start) > b {
				msg := "prism: upstream too slow, retry budget exhausted (please retry)"
				if entry != nil {
					entry.Error = msg
				}
				chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &msg}}},
					Error:   &ChunkError{Message: msg, Type: "upstream_error"}}
				ss.Event(chunk)
				fr := "stop"
				chunk = ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
				ss.Event(chunk)
				ss.Done()
				acc.Release()
				return
			}
			tPick := time.Now()
			acc, err = iter.Next()
			log.Printf("prism: t_pick attempt=%d account=%q inflight=%d latency_ms=%d err=%v", attempt, accName(acc), accInflight(acc), time.Since(tPick).Milliseconds(), err)
			if err != nil {
				// 无可用账号：SSE 头已发出，输出错误帧后结束
				// （acc 为 nil，上一个账号已在上面 Release，不能在这里再 Release）
				msg := err.Error()
				if entry != nil {
					entry.Error = msg
				}
				chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &msg}}},
					Error:   &ChunkError{Message: msg, Type: "upstream_error"}}
				ss.Event(chunk)
				fr := "stop"
				chunk = ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
				ss.Event(chunk)
				ss.Done()
				return
			}
		}

		sentContent := false // 是否已向客户端发出内容帧（Text/Thinking/ToolCall delta），决定能否换号重试
		// T1.0 首帧提前已撤（2026-09-21）：提前发 role 帧锁死 HTTP 200，relay 全失败
		// 时只能靠 error 帧收尾——NewAPI 不认 error 帧正文，客户看到"空回"（线上
		// 实锤：12s/completion=0/1288 次每40分钟）。首帧改到 relay 首个真实输出前
		// （ensureFirstFrame），失败发生在响应头生效前 → 返回真 5xx →
		// NewAPI 自动换渠道重试，客户不再看到空回。
		// 心跳：上游是轮询式整轮返回，推理类请求（codex high 档 1-3 分钟）期间
		// 连接完全静默，端上 App/中间层的空闲超时（普遍 180s）会把连接掐成
		// client_gone（线上 2026-09-20 实锤：received=2 后静默至 3m0s 整断）。
		// 每 15s 发一个空 delta 帧让全链路字节流动；内容开始后帧流本就密集，心跳无害。
		hbStop := make(chan struct{})
		defer close(hbStop)
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-hbStop:
					return
				case <-r.Context().Done():
					return
				case <-t.C:
					hb := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}}}}
					if ss.Event(hb) != nil {
						return
					}
				}
			}
		}()
		var toolSeq = map[string]int{}
		var outTokens int
		var lastCtx *adapter.ContextWindowStatus
		var gateText strings.Builder        // gate 命中时攒全文，结束时清洗后一次性发出
		toolCalled := false                 // 是否已发出工具调用（决定 finish_reason）
		clientTools := clientToolIndex(req) // 客户端声明的工具集（内置工具映射目标）
		var blockedCalls []string           // S1：上游调了但客户端没有的工具（吞帧，只记摘要）
		var pendingText strings.Builder     // 非 gate 路径正文缓冲（沙箱护栏要看全本轮工具调用再发）
		reinforced := false                 // 协议强化重试至多一次（判定见 sandbox_guard.go）
		salvaged := false                   // 交付追捞重试至多一次（与协议强化互斥，判定见 sandbox_guard.go）
		salvageRecovered := false           // 追捞轮已把内容带回正文：剪除护栏让路（再剪会弹多余的网关说明）
		ensureFirstFrame := func() {
			if sentFirst {
				return
			}
			sentFirst = true
			_ = ss.Event(first)
		}
		flushPendingText := func(guard bool) {
			if pendingText.Len() == 0 {
				return
			}
			ensureFirstFrame()
			t := pendingText.String()
			pendingText.Reset()
			// 图片附件轮豁免：附件链路（还原再查看）产生"已创建文件"类动作是设计内行为。
			if guard && !toolCalled && !roundHasImageAttachment(agentReq) && !salvageRecovered {
				var cleaned string
				var hit bool
				if clientTools.empty() {
					// 无 tools：只剪"假交付事故"（声称写文件+缺内容），教学/示例输出不动
					cleaned, hit = guardNoToolDeliveryText(t)
				} else {
					cleaned, hit = guardSandboxExecutedText(t)
				}
				if hit {
					log.Printf("chat stream sandbox-execution guard stripped text account=%s tools=%t", acc.Name, !clientTools.empty())
					t = cleaned
				}
			}
			if t == "" {
				return
			}
			// 打字机：上游整轮一次性到文，按 tps 区间随机速率切片平滑发出（见 typewriter.go）。
			emit := func(s string) {
				chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &s}}}}
				if ss.Event(chunk) == nil {
					sentContent = true
				}
			}
			lo, hi := streamTPSRange()
			typewrite(r.Context(), emit, t, lo, hi)
		}
		addOut := func(s string) { outTokens += estimateTokens(s) }
		onEvent := func(ev adapter.Event) bool {
			delta := Delta{}
			chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model}
			if ev.ContextWindow != nil {
				lastCtx = ev.ContextWindow
			}
			if ev.Thinking != nil && ev.Thinking.Text != "" {
				t := ev.Thinking.Text
				delta.ReasoningContent = &t
				addOut(ev.Thinking.Text)
			}
			if ev.Text != "" {
				if gateOn {
					gateText.WriteString(ev.Text)
					addOut(ev.Text)
				} else {
					// 延迟发出：等看清本轮有没有工具调用再决定过不过沙箱护栏
					// （prism 的正文本就是整轮结束后才到，缓冲零 TTFB 代价）。
					pendingText.WriteString(ev.Text)
					addOut(ev.Text)
					return true
				}
			}
			if ev.PartialToolCall != nil && !skipPartialToolStart(ev.PartialToolCall.Name) {
				idx := toolIndex(toolSeq, ev.PartialToolCall.ToolCallID)
				mappedName := mapOutgoingToolNameIdx(ev.PartialToolCall.Name, clientTools)
				if mappedName == "" {
					// 无名 partial 帧：不发空名 tool_calls delta（客户端会拒绝/崩溃）。
					return true
				}
				delta.ToolCalls = []ToolCallDelta{{
					Index: idx, ID: ev.PartialToolCall.ToolCallID, Type: "function",
					Function: ToolCallFunction{Name: mappedName},
				}}
				addOut(mappedName)
			}
			if ev.ToolCall != nil {
				idx := toolIndex(toolSeq, ev.ToolCall.ToolCallID)
				name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
					Kind:       ev.ToolCall.Kind,
					ToolName:   ev.ToolCall.Name,
					ToolCallID: ev.ToolCall.ToolCallID,
					ArgsJSON:   ev.ToolCall.RawArgs,
				}, clientTools)
				if ok {
					flushPendingText(false) // 工具调用走正道：缓冲的正文原样先出，不过护栏
					delta.ToolCalls = []ToolCallDelta{{
						Index: idx, ID: ev.ToolCall.ToolCallID, Type: "function",
						Function: ToolCallFunction{Name: name, Arguments: args},
					}}
					addOut(name + args)
					toolCalled = true
				} else if blocked != "" {
					// S1：客户端没有该工具——吞帧并记录，收尾时以受限块补进上下文。
					blockedCalls = append(blockedCalls, blocked)
					log.Printf("chat upstream tool blocked (client lacks tool): %s account=%s", blocked, acc.Name)
				}
			}
			chunk.Choices = []ChoiceChunk{{Index: 0, Delta: delta}}
			if delta.Content != nil || delta.ReasoningContent != nil || len(delta.ToolCalls) > 0 {
				// gate 命中时文本被攒起、此帧只发空 delta：内容帧的判定以实际发出的 delta 为准
				sentContent = true
			}
			if ss.Event(chunk) != nil {
				return false
			}
			return true
		}

		// runRound 发一轮上游流；协议强化重试（至多一次）复用它，本轮产出状态在每轮开头重置。
		runRound := func() error {
			toolSeq = map[string]int{}
			outTokens = 0
			lastCtx = nil
			gateText.Reset()
			toolCalled = false
			blockedCalls = nil
			pendingText.Reset()
			sentContent = false
			return acc.Client.Stream(r.Context(), agentReq, onEvent)
		}
		streamErr := runRound()
		// 协议强化重试：声明了工具却零 tool_call 且正文带"模型翻自己环境作答"的证据时，
		// 同账号加纠正块重发一轮。此刻正文还缓冲在 pendingText、未发任何内容帧
		// （思考帧一旦发出 sentContent=true 就不再重试），客户端不可见。
		if streamErr == nil && !toolCalled && !clientTools.empty() && !sentContent && !gateOn && !reinforced && toolContractRetryOn() && shouldRetryToolContract(pendingText.String()) {
			log.Printf("chat contract-reinforce retry (zero tool_call with own-env evidence) account=%s", acc.Name)
			reinforced = true
			if agentReq.Extra == nil {
				agentReq.Extra = map[string]any{}
			}
			agentReq.Extra["prism_tool_reinforce"] = "1"
			agentReq.Extra["prism_tool_reinforce_bad"] = pendingText.String()
			streamErr = runRound()
		}
		// 交付追捞重试：假交付事故（声称写文件+正文缺内容，不限是否有 tools）或无 tools 时
		// 翻自己环境后拒绝，同账号加纠正块重发一轮，把内容捞回正文。与协议强化互斥
		// （一次请求至多一轮重试），图片附件轮豁免，此刻正文仍在缓冲、客户端不可见。
		if streamErr == nil && !toolCalled && !sentContent && !gateOn && !reinforced && !salvaged && !roundHasImageAttachment(agentReq) && deliverySalvageRetryOn() {
			if mode, ok := planDeliverySalvage(pendingText.String(), clientTools.empty()); ok {
				log.Printf("chat delivery-salvage retry (mode=%s) account=%s tools=%t", mode, acc.Name, !clientTools.empty())
				salvaged = true
				if agentReq.Extra == nil {
					agentReq.Extra = map[string]any{}
				}
				agentReq.Extra["prism_delivery_salvage"] = mode
				agentReq.Extra["prism_delivery_salvage_bad"] = pendingText.String()
				streamErr = runRound()
				// 观测：追捞轮是否真把内容带回来了（still-accident 时下方剪除护栏兜底）。
				if streamErr == nil {
					switch {
					case sandboxDeliveryClaim(pendingText.String()):
						log.Printf("chat delivery-salvage still-accident account=%s mode=%s", acc.Name, mode)
					case pendingText.Len() > 0:
						salvageRecovered = true
						log.Printf("chat delivery-salvage recovered account=%s mode=%s len=%d", acc.Name, mode, pendingText.Len())
					default:
						log.Printf("chat delivery-salvage empty account=%s mode=%s", acc.Name, mode)
					}
				}
			}
		}

		if streamErr == nil && outTokens == 0 && len(blockedCalls) == 0 {
			// 此刻只发过 role 首帧（有任何内容帧 outTokens 就 >0），换号对客户端不可见。
			// S1 拦截收尾（blockedCalls 非空）是刻意行为，不在此重试。
			if emptyRetries < emptyRetryMax {
				emptyRetries++
				log.Printf("chat stream empty completion, switching account (retry %d/%d) account=%s model=%s", emptyRetries, emptyRetryMax, acc.Name, req.Model)
				acc.Release()
				continue
			}
			log.Printf("chat stream empty completion exhausted after %d retries account=%s model=%s", emptyRetryMax, acc.Name, req.Model)
			msg := "prism: upstream returned empty completion"
			if entry != nil {
				entry.Error = msg
			}
			chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &msg}}},
				Error:   &ChunkError{Message: msg, Type: "upstream_error"}}
			ss.Event(chunk)
			fr := "stop"
			chunk = ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
			ss.Event(chunk)
			ss.Done()
			acc.Release()
			return
		}
		// S1 说明帧计入 completion：整轮只有拦截说明送达时，不记会落成「0 t/s 无输出」假账
		// （usage 在下方 buildUsage 才汇总，计数必须发生在它之前；发出点不再重复计）。
		if note := blockedToolNote(blockedCalls); note != "" {
			addOut(note)
		}

		if streamErr != nil && !sentContent {
			res := s.noteAccount(acc, streamErr, req.Model, r.Context().Err() != nil, entry, iter)
			if !res.Switch {
				log.Printf("chat stream not retried (account=%s class=%s): %v", acc.Name, res.Class, streamErr)
				if res.Class == failclass.Canceled {
					acc.Release()
					return
				}
				msg := streamErr.Error()
				if entry != nil {
					entry.Error = msg
				}
				chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &msg}}},
					Error:   &ChunkError{Message: msg, Type: "upstream_error"}}
				ss.Event(chunk)
				fr := "stop"
				chunk = ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
				ss.Event(chunk)
				ss.Done()
				acc.Release()
				return
			}
			log.Printf("chat attempt %d failed on account %q class=%s: %v (retrying)", attempt+1, acc.Name, res.Class, streamErr)
			acc.Release()
			continue
		}
		if streamErr != nil {
			s.noteAccount(acc, streamErr, req.Model, r.Context().Err() != nil, entry, iter)
		} else {
			acc.MarkSuccess()
			fillLog(entry, acc, iter, "")
		}
		if entry != nil {
			entry.Account = acc.Name
		}

		usage := buildUsage(req, outTokens, lastCtx)
		s.applyChatCache(cachePlan, req.Model, usage, streamErr == nil)
		if entry != nil {
			entry.Prompt = usage.PromptTokens
			entry.Completion = usage.CompletionTokens
			entry.Total = usage.TotalTokens
		}
		if streamErr == nil {
			s.recordClientUsage(r, usage.PromptTokens, usage.CompletionTokens)
		}
		log.Printf("chat stream=true account=%s model=%s prompt=%d completion=%d total=%d ctx_used=%d ctx_limit=%d latency=%s",
			acc.Name, req.Model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
			ctxUsed(lastCtx), ctxLimit(lastCtx), time.Since(start).Round(time.Millisecond))

		if streamErr != nil {
			// 流中已发出数据，追加错误内容帧后结束
			msg := streamErr.Error()
			if entry != nil {
				entry.Error = msg
			}
			chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &msg}}},
				Error:   &ChunkError{Message: msg, Type: "upstream_error"}}
			ss.Event(chunk)
			fr := "stop"
			chunk = ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
			ss.Event(chunk)
			ss.Done()
			acc.Release()
			return
		}

		// 身份闸：攒下的全文在这里清洗后一次性发出（未命中时 gateText 为空）。
		if gateOn && gateText.Len() > 0 {
			ensureFirstFrame()
			final, action := s.guardIdentityAnswer(gateQuestion, gateText.String())
			logIdentityGuard(action, gateQuestion)
			ss.Event(ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &final}}}})
		}

		// 冲刷延迟正文：到此处仍无工具调用送达，过沙箱护栏（零 tool_call 才剪）。
		flushPendingText(true)

		// S1 收尾：流式路径正文已逐帧发出，模型文本无法回改；只补受限说明帧。
		// 客户端看到说明后，下一轮回灌/追问时模型已知"该工具在客户端不存在"。
		if note := blockedToolNote(blockedCalls); note != "" {
			ss.Event(ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []ChoiceChunk{{Index: 0, Delta: Delta{Content: &note}}}})
		}

		ensureFirstFrame()
		fr := "stop"
		if toolCalled {
			fr = "tool_calls"
		}
		chunk := ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []ChoiceChunk{{Index: 0, Delta: Delta{}, FinishReason: &fr}}}
		ss.Event(chunk)
		// 用量帧（OpenAI 惯例：choices 为空数组）
		ss.Event(ChatCompletionChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []ChoiceChunk{}, Usage: usage})
		ss.Done()
		acc.Release()
		return
	}
}

// nonStreamChat 非流式对话：收集全部输出后一次性返回。
// 连接层失败时自动切换到下一个账号重试（与 streamChat 一致）。
func (s *Server) nonStreamChat(w http.ResponseWriter, r *http.Request, req *ChatCompletionRequest, agentReq *adapter.NativeRequest, entry *admin.LogEntry, iter *accountIter, cachePlan *emulation.Plan) {
	start := time.Now()
	if stormFastFail(w, r, "openai") {
		return
	}
	acc, err := iter.Next()
	if err != nil {
		writePickError(w, err, false)
		return
	}

	emptyRetries := 0 // 空 completion 换号重试计数（跨 attempt 累计；语义见 streamChat 同名逻辑）

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// 重试总预算（同流式路径）：预算内返回干净错误，不吊死客户端到渠道超时。
			if b := retryBudget(); b > 0 && time.Since(start) > b {
				if entry != nil {
					entry.Error = "prism: retry budget exhausted"
				}
				acc.Release()
				writeOpenAIClassError(w, failclass.Result{Class: failclass.Server},
					"prism: upstream too slow, retry budget exhausted (please retry)")
				return
			}
			acc, err = iter.Next()
			if err != nil {
				// acc 为 nil（上一个账号上面已 Release），这里只报错。
				writePickError(w, err, false)
				return
			}
		}

		var text strings.Builder
		var thinking strings.Builder
		var toolCalls []ToolCall
		var lastCtx *adapter.ContextWindowStatus
		var blockedCalls []string // S1：上游调了但客户端没有的工具（吞帧，只记摘要）
		sentAny := false
		clientTools := clientToolIndex(req)
		reinforced := false       // 协议强化重试至多一次（判定见 sandbox_guard.go）
		salvaged := false         // 交付追捞重试至多一次（与协议强化互斥，判定见 sandbox_guard.go）
		salvageRecovered := false // 追捞轮已把内容带回正文：剪除护栏让路（同流式路径）

		onEvent := func(ev adapter.Event) bool {
			sentAny = true
			if ev.ContextWindow != nil {
				lastCtx = ev.ContextWindow
			}
			if ev.Thinking != nil {
				thinking.WriteString(ev.Thinking.Text)
			}
			text.WriteString(ev.Text)
			if ev.ToolCall != nil {
				name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
					Kind:       ev.ToolCall.Kind,
					ToolName:   ev.ToolCall.Name,
					ToolCallID: ev.ToolCall.ToolCallID,
					ArgsJSON:   ev.ToolCall.RawArgs,
				}, clientTools)
				if ok {
					toolCalls = append(toolCalls, ToolCall{
						ID: ev.ToolCall.ToolCallID, Type: "function",
						Function: ToolCallFunction{Name: name, Arguments: args},
					})
				} else if blocked != "" {
					blockedCalls = append(blockedCalls, blocked)
					log.Printf("chat upstream tool blocked (client lacks tool): %s account=%s", blocked, acc.Name)
				}
			}
			return true
		}

		// runRound 发一轮上游流；协议强化重试（至多一次）复用它，本轮产出状态在每轮开头重置。
		runRound := func() error {
			text.Reset()
			thinking.Reset()
			toolCalls = nil
			lastCtx = nil
			blockedCalls = nil
			sentAny = false
			return acc.Client.Stream(r.Context(), agentReq, onEvent)
		}
		streamErr := runRound()
		// 协议强化重试：声明了工具却零 tool_call 且正文带"模型翻自己环境作答"的证据时，
		// 同账号加纠正块重发一轮。非流式未向客户端发任何字节，重试不可见。
		if streamErr == nil && len(toolCalls) == 0 && !clientTools.empty() && !reinforced && toolContractRetryOn() && shouldRetryToolContract(text.String()) {
			log.Printf("chat contract-reinforce retry (zero tool_call with own-env evidence) account=%s", acc.Name)
			reinforced = true
			if agentReq.Extra == nil {
				agentReq.Extra = map[string]any{}
			}
			agentReq.Extra["prism_tool_reinforce"] = "1"
			agentReq.Extra["prism_tool_reinforce_bad"] = text.String()
			streamErr = runRound()
		}
		// 交付追捞重试：假交付事故（不限 tools）或无 tools 时翻自己环境后拒绝，同账号加
		// 纠正块重发一轮把内容捞回正文。与协议强化互斥，图片附件轮豁免（同流式路径）。
		if streamErr == nil && len(toolCalls) == 0 && !reinforced && !salvaged && !roundHasImageAttachment(agentReq) && deliverySalvageRetryOn() {
			if mode, ok := planDeliverySalvage(text.String(), clientTools.empty()); ok {
				log.Printf("chat delivery-salvage retry (mode=%s) account=%s tools=%t", mode, acc.Name, !clientTools.empty())
				salvaged = true
				if agentReq.Extra == nil {
					agentReq.Extra = map[string]any{}
				}
				agentReq.Extra["prism_delivery_salvage"] = mode
				agentReq.Extra["prism_delivery_salvage_bad"] = text.String()
				streamErr = runRound()
				if streamErr == nil {
					switch {
					case sandboxDeliveryClaim(text.String()):
						log.Printf("chat delivery-salvage still-accident account=%s mode=%s", acc.Name, mode)
					case text.Len() > 0:
						salvageRecovered = true
						log.Printf("chat delivery-salvage recovered account=%s mode=%s len=%d", acc.Name, mode, text.Len())
					default:
						log.Printf("chat delivery-salvage empty account=%s mode=%s", acc.Name, mode)
					}
				}
			}
		}

		if streamErr == nil && text.Len() == 0 && thinking.Len() == 0 && len(toolCalls) == 0 && len(blockedCalls) == 0 {
			// 上游假完成：整轮零产出。非流式未发任何字节，换号重试完全不可见；
			// S1 拦截收尾（blockedCalls 非空）会补受限说明，是刻意行为，不在此重试。
			if emptyRetries < emptyRetryMax {
				emptyRetries++
				log.Printf("chat empty completion, switching account (retry %d/%d) account=%s model=%s", emptyRetries, emptyRetryMax, acc.Name, req.Model)
				acc.Release()
				continue
			}
			log.Printf("chat empty completion exhausted after %d retries account=%s model=%s", emptyRetryMax, acc.Name, req.Model)
			msg := "prism: upstream returned empty completion"
			if entry != nil {
				entry.Error = msg
			}
			acc.Release()
			writeOpenAIClassError(w, failclass.Result{Class: failclass.Server}, msg)
			return
		}

		if streamErr != nil && !sentAny {
			res := s.noteAccount(acc, streamErr, req.Model, r.Context().Err() != nil, entry, iter)
			if !res.Switch {
				log.Printf("chat not retried (account=%s class=%s): %v", acc.Name, res.Class, streamErr)
				if res.Class == failclass.Canceled {
					acc.Release()
					return
				}
				if entry != nil {
					entry.Error = streamErr.Error()
				}
				acc.Release()
				writeOpenAIClassError(w, res, streamErr.Error())
				return
			}
			log.Printf("chat attempt %d failed on account %q class=%s: %v (retrying)", attempt+1, acc.Name, res.Class, streamErr)
			acc.Release()
			continue
		}
		var mid failclass.Result
		if streamErr != nil {
			mid = s.noteAccount(acc, streamErr, req.Model, r.Context().Err() != nil, entry, iter)
		} else {
			acc.MarkSuccess()
			fillLog(entry, acc, iter, "")
		}
		// S1 收尾：发生拦截时，剪掉模型翻空沙箱的自证，补受限说明。
		finalText := text.String()
		if note := blockedToolNote(blockedCalls); note != "" {
			finalText = stripEmptyWorkspaceClaims(finalText)
			finalText += "\n\n" + note
		}
		// 沙箱代执行护栏：零 tool_call 送达时清理"上游在自己沙箱里执行/创建"的假动作。
		// 有 tools：宽护栏（句级剪假动作声称+ls 清单）；无 tools：窄护栏（只动假交付事故，
		// 声称写文件+缺内容）；图片附件轮豁免（还原再查看是设计内行为）；
		// 追捞已带回内容时让路（salvageRecovered，同流式路径）。
		if len(toolCalls) == 0 && !roundHasImageAttachment(agentReq) && !salvageRecovered {
			var cleaned string
			var hit bool
			if clientTools.empty() {
				cleaned, hit = guardNoToolDeliveryText(finalText)
			} else {
				cleaned, hit = guardSandboxExecutedText(finalText)
			}
			if hit {
				log.Printf("chat sandbox-execution guard stripped text account=%s tools=%t", acc.Name, !clientTools.empty())
				finalText = cleaned
			}
		}
		// 身份闸：只在身份话题改写正文，普通问答 text 原样（usage 随之用改后文本）。
		if s.runtimeIdentityGuardOn() {
			if question, ok := identityGate(req); ok {
				var action string
				finalText, action = s.guardIdentityAnswer(question, finalText)
				logIdentityGuard(action, question)
			}
		}
		msg := ChatMessage{Role: "assistant", Content: json.RawMessage(mustJSON(finalText))}
		if thinking.Len() > 0 {
			msg.Reasoning = thinking.String()
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		if streamErr != nil {
			if entry != nil {
				entry.Error = streamErr.Error()
			}
			acc.Release()
			writeOpenAIClassError(w, mid, streamErr.Error())
			return
		}
		usage := buildUsage(req, estimateTokens(finalText+thinking.String()+toolArgs(toolCalls)), lastCtx)
		s.applyChatCache(cachePlan, req.Model, usage, true)
		if entry != nil {
			entry.Prompt = usage.PromptTokens
			entry.Completion = usage.CompletionTokens
			entry.Total = usage.TotalTokens
		}
		s.recordClientUsage(r, usage.PromptTokens, usage.CompletionTokens)
		log.Printf("chat stream=false account=%s model=%s prompt=%d completion=%d total=%d ctx_used=%d ctx_limit=%d latency=%s",
			acc.Name, req.Model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
			ctxUsed(lastCtx), ctxLimit(lastCtx), time.Since(start).Round(time.Millisecond))
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()), "object": "chat.completion",
			"created": time.Now().Unix(), "model": req.Model, "choices": []map[string]any{{
				"index": 0, "message": msg, "finish_reason": nonStreamFinishReason(len(toolCalls) > 0),
			}}, "usage": usage,
		})
		acc.Release()
		return
	}
}

// handleLogin 发起浏览器登录（账号池模式），返回登录 URL；后台轮询成功后账号入池并持久化。
// GET /v1/login → 默认账号 "default"；GET /v1/login?name=xxx → 指定账号名。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "default"
	}
	url, uuid, err := s.pool.StartBrowserLogin(name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	if s.cfg.OpenBrowser {
		openBrowser(url)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"login_url": url, "uuid": uuid, "name": name,
		"message": "open the URL in a browser and complete login; tokens will be stored automatically",
	})
}

// openBrowser 用系统默认浏览器打开登录 URL（跨平台）。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("warn: open browser: %v", err)
	}
}

// handleAuthStatus 返回登录状态（默认账号；?name=xxx 指定账号）。
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "default"
	}
	acc := s.pool.Get(name)
	if acc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"logged_in": false, "name": name, "detail": "account not found"})
		return
	}
	snap := acc.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"logged_in":   snap.LoggedIn,
		"name":        name,
		"expires_at":  snap.ExpiresAt,
		"has_api_key": snap.HasAPIKey,
	})
}

// handleAPIKey 添加/更新一个 API Key 账号（默认 name=default；?name=xxx 指定账号名）。
func (s *Server) handleAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"api_key"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&body); err != nil || body.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "api_key is required"}})
		return
	}
	name := body.Name
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		name = "default"
	}
	if err := s.pool.AddAPIKey(name, body.APIKey); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	log.Printf("account %q: api key added", name)
	writeJSON(w, http.StatusOK, map[string]any{"logged_in": true, "name": name})
}

// handleAccounts 账号池管理：
//   - GET  /v1/accounts           → 全部账号状态列表
//   - POST /v1/accounts           → 新增账号（body: {name?, api_key?}；有 api_key 直接入池，否则返回浏览器登录 URL）
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snaps := s.pool.List()
		if snaps == nil {
			snaps = []pool.Snapshot{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": snaps, "total": len(snaps)})
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(ioLimit(r.Body)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid body: " + err.Error()}})
			return
		}
		name := body.Name
		if name == "" {
			name = "default"
		}
		if body.APIKey != "" {
			if err := s.pool.AddAPIKey(name, body.APIKey); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"name": name, "logged_in": true})
			return
		}
		url, uuid, err := s.pool.StartBrowserLogin(name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "login_url": url, "uuid": uuid})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"message": "method not allowed"}})
	}
}

// handleAccountDelete 删除账号：DELETE /v1/accounts/{name}。
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"message": "method not allowed, use DELETE"}})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/accounts/")
	if name == "" || !validAccountName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid account name"}})
		return
	}
	if err := s.pool.Remove(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	log.Printf("account %q removed", name)
	writeJSON(w, http.StatusOK, map[string]any{"removed": name})
}

// logEntryFrom 从请求上下文取日志条目（middleware 注入）。
func logEntryFrom(r *http.Request) *admin.LogEntry {
	e, _ := r.Context().Value(ctxLogKey{}).(*admin.LogEntry)
	return e
}

// --- 小工具 ---

// validAccountName 校验账号名（与 pool 内规则一致）。
func validAccountName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// buildUsage 汇总用量：上游返回 context_window_status 时 PromptTokens 取真实上下文占用，
// 否则回退本地估算；同时透传 context_window_status 原始值。
func buildUsage(req *ChatCompletionRequest, outTokens int, ctx *adapter.ContextWindowStatus) *Usage {
	prompt := estimateInputTokens(req)
	if ctx != nil && ctx.TokensUsed > 0 {
		prompt = int(ctx.TokensUsed)
	}
	return &Usage{
		PromptTokens:     prompt,
		CompletionTokens: outTokens,
		TotalTokens:      prompt + outTokens,
		ContextWindow:    ctx,
	}
}

// applyChatCache 把模拟缓存的读命中落到 OpenAI usage（prompt_tokens_details.cached_tokens）。
// 官方语义：cached ⊆ prompt_tokens，总额不动；只有读命中 >0 才出现该字段（与官方一致——
// 首轮全量写入时不带 details）。success=true 才 Commit 前缀（失败请求不污染前缀表）。
func (s *Server) applyChatCache(plan *emulation.Plan, model string, usage *Usage, success bool) {
	if plan == nil || usage == nil {
		return
	}
	cu := plan.Result()
	if cu != nil && cu.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: cu.CacheReadInputTokens}
	}
	if success {
		plan.Commit()
		if s.emu != nil {
			s.emu.NoteCache(model, usage.PromptTokens, cu)
		}
	}
}
func ctxUsed(ctx *adapter.ContextWindowStatus) int64 {
	if ctx == nil {
		return 0
	}
	return ctx.TokensUsed
}

func ctxLimit(ctx *adapter.ContextWindowStatus) int64 {
	if ctx == nil {
		return 0
	}
	return ctx.TokenLimit
}

// estimateTokens 估算字符串的 token 数：ASCII 约 4 字符/token，CJK 等宽字符约 1 rune/token（近似）。
func estimateTokens(s string) int {
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	n := ascii/4 + wide
	if n < 1 && s != "" {
		return 1
	}
	return n
}

// estimateInputTokens 估算请求输入 token：messages + tools 序列化后的近似值。
func estimateInputTokens(req *ChatCompletionRequest) int {
	b, _ := json.Marshal(struct {
		Messages []ChatMessage `json:"messages"`
		Tools    []Tool        `json:"tools,omitempty"`
		Model    string        `json:"model"`
	}{req.Messages, req.Tools, req.Model})
	t := estimateTokens(string(b))
	if t < 1 {
		t = 1
	}
	return t
}

// toolArgs 拼接全部工具调用的参数（用于输出 token 估算）。
func toolArgs(calls []ToolCall) string {
	var sb strings.Builder
	for _, c := range calls {
		sb.WriteString(c.Function.Name)
		sb.WriteString(c.Function.Arguments)
	}
	return sb.String()
}

func toolIndex(seq map[string]int, id string) int {
	if id == "" {
		return 0
	}
	if i, ok := seq[id]; ok {
		return i
	}
	seq[id] = len(seq)
	return seq[id]
}

// newUUID 生成 UUID v4（标准库实现，无外部依赖）。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("mock-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func pollBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 0; i < attempt && i < 10; i++ {
		d = time.Duration(float64(d) * 1.2)
	}
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func ioLimit(r interface{ Read([]byte) (int, error) }) io.Reader {
	return io.LimitReader(r, 32<<20)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// logMiddleware 记录请求日志（状态码、耗时、账号/模型/用量由 handler 经 context 补充）。
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipAccessLog(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rid := requestIDFrom(r)
		entry := &admin.LogEntry{
			Time:      time.Now(),
			IP:        clientIP(r),
			Method:    r.Method,
			Path:      r.URL.Path,
			RequestID: rid,
			Headers:   admin.SanitizeHeaders(r.Header),
		}
		// M1 速率计量 + M2 入口闸门：/v1/* 先计数再拿并发槽；
		// 排队超时回 429（不调 next，access-log/计量经下方收尾自然覆盖）。
		isAPI := strings.HasPrefix(r.URL.Path, "/v1/")
		if isAPI {
			s.meters.Incoming()
		}
		out := http.ResponseWriter(w)
		var dbg *debugtrace.Session
		if debugtrace.Enabled() && !skipDebugTrace(r.URL.Path) {
			var body []byte
			if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
				body, _ = io.ReadAll(io.LimitReader(r.Body, 32<<20))
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			dbg = debugtrace.Start(rid, r.Method, r.URL.Path, r.URL.RawQuery, admin.SanitizeHeaders(r.Header), body)
			if dbg != nil {
				log.Printf("[debug] %s %s -> %s", r.Method, r.URL.Path, dbg.Path)
				out = &debugBody{ResponseWriter: w, sess: dbg}
			}
		}
		rec := &statusRecorder{ResponseWriter: out, status: http.StatusOK}
		rec.Header().Set("X-Request-ID", rid)
		capBody := wrapBodyCapture(r)
		ctx := context.WithValue(r.Context(), ctxLogKey{}, entry)
		if dbg != nil {
			ctx = debugtrace.With(ctx, dbg)
		}
		if isAPI {
			if gch, ok := s.gateAcquire(ctx); ok {
				if gch != nil { // nil=闸门未启用（空池/未配容量），直通不占槽
					defer func() { <-gch }() // 释放捕获的获取时 channel（resize 安全）
				}
			} else {
				rec.Header().Set("Retry-After", strconv.Itoa(gateRetryAfterSec))
				writeJSON(rec, http.StatusTooManyRequests, map[string]any{
					"error": map[string]string{
						"message": "server at capacity, retry shortly",
						"type":    "rate_limit_error",
					},
				})
				return
			}
		}
		next.ServeHTTP(rec, r.WithContext(ctx))
		entry.Status = rec.status
		entry.Latency = time.Since(start).Milliseconds()
		if capBody != nil {
			entry.RequestBody = capBody.loggedBody()
		}
		if entry.Error == "" && rec.status >= 400 {
			entry.Error = http.StatusText(rec.status)
		}
		entry.Error = admin.RedactText(entry.Error)
		if dbg != nil {
			dbg.Finish(map[string]any{
				"status":  entry.Status,
				"account": entry.Account,
				"model":   entry.Model,
				"error":   entry.Error,
				"latency": entry.Latency,
			})
		}
		s.logs.Add(*entry)
		if isAPI {
			s.meters.Done(entry.Model, entry.Status, entry.Prompt, entry.Completion, entry.RetryCount)
		}
		log.Printf("%s %s (%s) account=%s model=%s request_id=%s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), entry.Account, entry.Model, entry.RequestID)
	})
}

func skipAccessLog(path string) bool {
	switch path {
	case "/healthz", "/api/hello", "/api/admin/events", "/api/admin/tasks/events":
		return true
	}
	if strings.HasPrefix(path, "/debug/") {
		return true
	}
	if strings.HasPrefix(path, "/api/admin/tasks/") && strings.HasSuffix(path, "/events") {
		return true
	}
	return false
}

func skipDebugTrace(path string) bool {
	if skipAccessLog(path) {
		return true
	}
	if strings.HasPrefix(path, "/admin") || strings.HasPrefix(path, "/assets") {
		return true
	}
	return false
}

// debugBody 把写出的 HTTP/SSE 字节记进 debug JSONL（兼容 Flush）。
type debugBody struct {
	http.ResponseWriter
	sess   *debugtrace.Session
	header bool
}

func (w *debugBody) WriteHeader(code int) {
	if !w.header {
		w.header = true
		w.sess.Emit("http_status", map[string]any{
			"code":    code,
			"headers": debugtrace.RedactHeaders(w.Header()),
		})
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *debugBody) Write(p []byte) (int, error) {
	if !w.header {
		w.WriteHeader(http.StatusOK)
	}
	w.sess.Emit("http_write", string(p))
	return w.ResponseWriter.Write(p)
}

func (w *debugBody) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *debugBody) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusRecorder 包装 ResponseWriter 记录最终状态码（兼容 SSE Flush）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ctxLogKey 请求日志上下文键。
type ctxLogKey struct{}

// clientIP 提取客户端 IP（去掉端口）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
