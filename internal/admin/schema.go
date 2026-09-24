package admin

// FieldSchema 热配置字段元数据（本批只出 JSON，不出表单 UI）。
type FieldSchema struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	RestartRequired bool   `json:"restart_required"`
	Secret          bool   `json:"secret,omitempty"`
	Default         any    `json:"default,omitempty"`
	Description     string `json:"description"`
}

// ConfigSchema 运行时配置字段表。
func ConfigSchema() []FieldSchema {
	return []FieldSchema{
		{Name: "api_base_url", Type: "string", Description: "上游 API 地址"},
		{Name: "website_url", Type: "string", Description: "上游网站地址（登录）"},
		{Name: "api_key_auth", Type: "string", Secret: true, Description: "本站入口 Key；空=不鉴权"},
		{Name: "cache_ttl_seconds", Type: "int", Default: 600, Description: "模型列表缓存秒数"},
		{Name: "log_max_entries", Type: "int", Default: 5000, Description: "内存环形日志条数"},
		{Name: "log_retention_days", Type: "int", Default: 7, Description: "PostgreSQL 追踪保留天数"},
		{Name: "listen_addr", Type: "string", RestartRequired: true, Description: "监听地址"},
		{Name: "mock_mode", Type: "bool", RestartRequired: true, Description: "内置 mock 后端"},
		{Name: "admin_username", Type: "string", Description: "管理员用户名"},
		{Name: "admin_password", Type: "string", Secret: true, Description: "改管理员密码（只写）"},
		{Name: "routing_strategy", Type: "string", Default: "round-robin", Description: "round-robin | weighted-round-robin | fill-first"},
		{Name: "session_affinity", Type: "bool", Default: true, Description: "会话粘性"},
		{Name: "session_affinity_ttl", Type: "string", Default: "1h", Description: "粘性 TTL"},
		{Name: "request_retry", Type: "int", Default: 3, Description: "外层等待冷却再试次数"},
		{Name: "max_retry_credentials", Type: "int", Default: 0, Description: "单请求最多试号数，0=全部"},
		{Name: "max_retry_interval", Type: "int", Default: 30, Description: "最长等待秒数"},
		{Name: "disable_cooling", Type: "bool", Default: false, Description: "关闭冷却"},
		{Name: "allow_remote_admin", Type: "bool", Default: false, Description: "允许非本机访问 /api/admin"},
		{Name: "account_concurrency", Type: "int", Default: 0, Description: "单号并发上限；0=不限制（本上游无速率限制，默认不限）"},
		{Name: "account_concurrency_429", Type: "int", Default: 0, Description: "账号出现 429 后并发降至此值；0=不降级"},
		{Name: "prewarm_accounts", Type: "int", Default: 16, Description: "warm-set 容量（按 MRU 取前 K 个启用账号预热）；0=关闭预热"},
		{Name: "prewarm_interval_sec", Type: "int", Default: 60, Description: "预热巡检间隔秒（10~600）；沙箱 TTL 2 分钟，刷新须在过期前完成"},
		{Name: "warm_preference", Type: "bool", Default: false, Description: "温热优先选号：优先挑 sandbox 预热的账号，降低首字延迟（TTFB L1）"},
		{Name: "proxy_url", Type: "string", Description: "全局上游 HTTP 正代；账号 proxy_url / 代理池优先。跨机走域名时请用 Resin 反代，不要配 CONNECT/SOCKS"},
		{Name: "resin_enabled", Type: "bool", Default: false, Description: "启用全局 Resin 反向代理出口（账号未绑定代理池条目时）"},
		{Name: "resin_url", Type: "string", Description: "Resin 实例根地址，含 token 路径，例如 https://resin.example.com/<proxy_token>"},
		{Name: "resin_platform", Type: "string", Default: "Default", Description: "Resin 平台名，通常为 Default"},
		{Name: "batch_concurrency", Type: "int", Default: 4, Description: "管理台批量 CRUD / 刷新 / 探测的全局并发"},
		{Name: "model_routes", Type: "map", Description: "模型名路由表，接到 CleanAnthropicModelName"},
		{Name: "history_compress", Type: "string", Default: "code", Description: "off | light | code | standard | aggressive"},
		{Name: "system_prompt", Type: "text", Description: "注入上游的系统指令；空=内置默认"},
		{Name: "system_prompt_mode", Type: "string", Default: "inject", Description: "inject | off（off=不注入，回到上游默认）"},
		{Name: "identity_guard_enabled", Type: "bool", Default: true, Description: "身份闸：身份话题才清洗漏网的底座自述，普通问答不动"},
		{Name: "identity_answer_text", Type: "text", Description: "身份闸回退答句；空=默认答句"},
	}
}
