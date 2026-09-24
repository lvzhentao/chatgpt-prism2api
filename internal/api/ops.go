package api

import (
	"log"
	"net"
	"net/http"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/admin"
	"prism-2api/internal/failclass"
	"prism-2api/internal/pg"
)

func (s *Server) adminRemoteAllowed(r *http.Request) bool {
	if s.runtime != nil && s.runtime.AllowRemote() {
		return true
	}
	if listenIsLoopback(s) {
		return true
	}
	return isLoopbackAddr(r.RemoteAddr)
}

func listenIsLoopback(s *Server) bool {
	addr := ""
	if s.runtime != nil {
		if v, ok := s.runtime.Get()["listen_addr"].(string); ok {
			addr = v
		}
	}
	if addr == "" && s.cfg != nil {
		addr = s.cfg.ListenAddr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) rejectRemoteAdmin(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": map[string]string{"message": "remote admin disabled; set allow_remote_admin", "type": "auth_error"},
	})
}

func (s *Server) mergedModelRoutes() map[string]string {
	routes := map[string]string{}
	if s.cfg != nil && s.cfg.ModelMap != nil {
		for k, v := range s.cfg.ModelMap {
			routes[k] = v
		}
	}
	if s.runtime != nil {
		for k, v := range s.runtime.ModelRouteMap() {
			routes[k] = v
		}
	}
	return routes
}

func (s *Server) resolveModel(name string) string {
	def := ""
	if s.cfg != nil {
		def = s.cfg.DefaultModel
	}
	return CleanAnthropicModelName(name, s.mergedModelRoutes(), def)
}

func (s *Server) applyRuntimeSideEffects() {
	if s.runtime == nil {
		return
	}
	ApplyHistoryCompress(s.runtime.HistoryTier())
	proxy := s.runtime.GlobalProxy()
	if proxy == "" && s.cfg != nil {
		proxy = s.cfg.ProxyURL
	}
	if s.pool != nil {
		s.pool.SetGlobalProxy(proxy)
		s.pool.SetEgressResolver(s.resolveAccountEgress)
	}
	if s.jobs != nil {
		s.jobs.SetConcurrency(s.runtime.BatchConcurrencyN())
	}
}

func (s *Server) purgeLogsLoop() {
	if s.pg == nil {
		return
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	purge := func() {
		days := pg.DefaultRetention
		if s.runtime != nil {
			days = s.runtime.RetentionDays()
		}
		n, err := s.pg.PurgeOlderThan(days)
		if err != nil {
			log.Printf("pg purge logs: %v", err)
			return
		}
		if n > 0 {
			log.Printf("pg purged %d request_logs older than %d days", n, days)
		}
	}
	purge()
	for range tick.C {
		purge()
	}
}

func retryAfterSeconds(class failclass.Class) int {
	switch class {
	case failclass.RateLimit:
		return 60
	case failclass.Server:
		return 120
	case failclass.Unavailable:
		return 300
	default:
		return 0
	}
}

func officialErrorCode(c failclass.Class) string {
	switch c {
	case failclass.RateLimit:
		return "rate_limit_exceeded"
	case failclass.Auth:
		return "invalid_api_key"
	case failclass.Quota:
		return "insufficient_quota"
	case failclass.Region:
		return "region_not_supported"
	case failclass.BadRequest:
		return "invalid_request"
	case failclass.Canceled:
		return "request_canceled"
	default:
		return "upstream_error"
	}
}

func setRetryAfter(w http.ResponseWriter, class failclass.Class) {
	if sec := retryAfterSeconds(class); sec > 0 {
		w.Header().Set("Retry-After", itoa(sec))
	}
}

// stormFastFail 上游风暴熔断的入口闸：开闸时（探测名额之外）快速 503 +
// Retry-After，不烧账号重试链（风暴里客户端平均白等 204s，2026-09-20 实锤）。
// 状态由 prism 适配器上报（adapter.StormRecord），判定见 adapter/storm.go。
// proto 取 "openai" / "anthropic" / "gemini"，各用自家错误形状。
// 返回 true 表示已写拒绝响应，调用方直接 return。
func stormFastFail(w http.ResponseWriter, r *http.Request, proto string) bool {
	if adapter.StormAllow() {
		return false
	}
	const msg = "prism: upstream storm detected, fast fail (please retry shortly)"
	// Retry-After 对齐探测间隔（15s）而非 server 类冷却基准（120s）：风暴是
	// 分钟级摆动，熔断自己每 15s 探测恢复，客户端（NewAPI 按此 park 渠道）
	// 也该分钟级回来；120s 会把好分钟也睡过去（2026-09-20 第四场风暴实锤）。
	w.Header().Set("Retry-After", "15")
	switch proto {
	case "anthropic":
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", msg)
	case "gemini":
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"code": 503, "message": msg, "status": "UNAVAILABLE"},
		})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "upstream_error",
				"code":    officialErrorCode(failclass.Server),
			},
		})
	}
	log.Printf("prism: storm fast-fail %s %s", r.Method, r.URL.Path)
	return true
}
func handleAdminConfigSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"fields": admin.ConfigSchema()})
}
