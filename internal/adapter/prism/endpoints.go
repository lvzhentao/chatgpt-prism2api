package prism

import "prism-2api/internal/adapter"

// ============================================================
// 定制点 1 — prism.openai.com 路径（全部来自 HAR 抓包）。
// Origin = .env 的 VENDOR_API_BASE_URL / VENDOR_WEBSITE_URL。
// 认证不是 Bearer，而是两个 cookie（见 client.go 的 cookie()）。
// ============================================================

const (
	// 对话：start + status 轮询模型（不是 SSE）。
	PathChat   = "/api/llm/response_with_tools_start"
	PathStatus = "/api/llm/response_with_tools_status"

	// 会话：用 prism_oai_access_token 换 prism_session_token。
	PathSession = "/auth/session"
	// 权益：planType / workspace。
	PathEntitlements = "/auth/entitlements"

	// 项目。
	PathProjects      = "/api/projects"
	PathProjectList   = "/api/file-management/projects?section=your_projects"
	PathProjectAccess = "/api/project-access?d="

	// 沙箱预热链（缺一步 start 会无限挂起）。
	PathBackendNew     = "/api/backend/1/new"
	PathResourcesToken = "/api/projects/%s/sandbox/resources-token" // %s = projectId
	// 以下三条拼在沙箱基址之后（基址 = backend/1/new 返回的 url）。
	PathProxyResources = "/resources-token"
	PathProxyToken     = "/token"
	PathWaitForSync    = "/wait-for-sync?wait_ms=10000"
	PathSandboxBase    = "/s/sandboxes/proxy"
	PathYSweet         = "/api/y"
	PathResourceBase   = "/s/sandbox-resources/"

	// 会话登记 / 调试（站点行为，保留为 best-effort）。
	PathConversationHistory = "/api/codex/conversation-history"
	PathRuntimeDebug        = "/api/codex/runtime/debug?conversation_id="

	// 模型目录：上游没有列表接口，客户端返回 ErrNotImplemented，目录走静态表。
	PathModels = "/v1/TODO/models"
	// 登录：站点只支持 OpenAI OAuth 弹窗，账号走管理台 cookie 导入。
	PathLogin  = ""
	PathSignup = "/v1/TODO/signup"

	// ChatStream=false：上游是「start + 轮询 status」，不是 SSE。
	ChatStream = false
)

func chatURL(apiBase string) string    { return adapter.JoinURL(apiBase, PathChat) }
func statusURL(apiBase string) string  { return adapter.JoinURL(apiBase, PathStatus) }
func modelsURL(apiBase string) string  { return adapter.JoinURL(apiBase, PathModels) }
func refreshURL(apiBase string) string { return adapter.JoinURL(apiBase, PathSession) }
func usageURL(apiBase string) string   { return adapter.JoinURL(apiBase, PathEntitlements) }
func loginURL(website string) string   { return adapter.JoinURL(website, PathLogin) }
