package api

import (
	"net/http"

	"prism-2api/internal/adminapi"
)

// mountAdminJSON 注册 /api/admin/* JSON，供独立前端调用。
// 已有 config/logs/stats/accounts GET·POST 仍走 admin.go，此处不重复注册。
func (s *Server) mountAdminJSON(mux *http.ServeMux) {
	h := &adminapi.Handler{
		Pool:               s.pool,
		Logs:               s.logs,
		Keys:               newClientKeyFacade(s.keys),
		Groups:             s.groups,
		Started:            s.started,
		Keepalive:          s.ka,
		WebsiteURL:         s.websiteURL(),
		AfterAccountChange: s.pool.RefreshAccountEgress,
		BatchConcurrency: func() int {
			if s.jobs != nil {
				return s.jobs.Concurrency()
			}
			if s.runtime != nil {
				return s.runtime.BatchConcurrencyN()
			}
			return 4
		},
	}
	authz := s.adminAuth

	// 机器导入端点走 API Key 鉴权（注册机推送用），与管理台会话鉴权分开。
	mux.HandleFunc("POST /api/v1/accounts/import", s.requireAPIKey(s.handleMachineImport(h)))

	mux.HandleFunc("GET /api/admin/me", authz(s.handleAdminMe))
	mux.HandleFunc("GET /api/admin/dashboard", authz(h.Dashboard))
	mux.HandleFunc("GET /api/admin/events", authz(h.Events))
	mux.HandleFunc("GET /api/admin/usage", authz(h.Usage))
	mux.HandleFunc("POST /api/admin/usage", authz(h.Usage))
	mux.HandleFunc("POST /api/admin/usage/refresh", authz(s.handleAdminUsageTask))
	mux.HandleFunc("POST /api/admin/accounts/{name}/usage/refresh", authz(h.RefreshAccountUsage))

	mux.HandleFunc("GET /api/admin/client-keys", authz(h.ListClientKeys))
	mux.HandleFunc("POST /api/admin/client-keys", authz(h.CreateClientKey))
	mux.HandleFunc("GET /api/admin/client-keys/{id}", authz(h.GetClientKey))
	mux.HandleFunc("PUT /api/admin/client-keys/{id}", authz(h.UpdateClientKey))
	mux.HandleFunc("PATCH /api/admin/client-keys/{id}", authz(h.UpdateClientKey))
	mux.HandleFunc("DELETE /api/admin/client-keys/{id}", authz(h.DeleteClientKey))

	mux.HandleFunc("GET /api/admin/groups", authz(h.ListGroups))
	mux.HandleFunc("POST /api/admin/groups", authz(h.CreateGroup))
	mux.HandleFunc("GET /api/admin/groups/{name}", authz(h.GetGroup))
	mux.HandleFunc("PATCH /api/admin/groups/{name}", authz(h.UpdateGroup))
	mux.HandleFunc("DELETE /api/admin/groups/{name}", authz(h.DeleteGroup))

	mux.HandleFunc("PATCH /api/admin/accounts", authz(h.PatchAccounts))
	mux.HandleFunc("GET /api/admin/accounts/export", authz(h.ExportAccounts))
	mux.HandleFunc("POST /api/admin/accounts/import", authz(s.handleAdminImportTask))
	mux.HandleFunc("POST /api/admin/accounts/actions", authz(s.handleAdminAccountActions))
	mux.HandleFunc("GET /api/admin/accounts/{name}", authz(s.handleAdminAccountGet))
	mux.HandleFunc("DELETE /api/admin/accounts/{name}", authz(s.handleAdminAccountDelete))

	mux.HandleFunc("GET /api/admin/models", authz(s.handleAdminModels))
	mux.HandleFunc("POST /api/admin/models/refresh", authz(s.handleAdminModelsTask))

	mux.HandleFunc("GET /api/admin/metrics", authz(s.handleAdminMetrics))
	mux.HandleFunc("GET /api/admin/metrics/view", authz(s.handleAdminMetricsView))

	mux.HandleFunc("GET /api/admin/pool/accounts", authz(s.handleAdminPoolAccounts))
	mux.HandleFunc("GET /api/admin/pool/view", authz(s.handleAdminPoolView))

	mux.HandleFunc("GET /api/admin/batches", authz(s.handleAdminBatches))
	mux.HandleFunc("GET /api/admin/batches/{id}", authz(s.handleAdminBatchGet))

	mux.HandleFunc("GET /api/admin/proxies", authz(s.handleAdminProxies))
	mux.HandleFunc("POST /api/admin/proxies", authz(s.handleAdminProxies))
	mux.HandleFunc("GET /api/admin/proxies/{id}", authz(s.handleAdminProxyOne))
	mux.HandleFunc("PATCH /api/admin/proxies/{id}", authz(s.handleAdminProxyOne))
	mux.HandleFunc("PUT /api/admin/proxies/{id}", authz(s.handleAdminProxyOne))
	mux.HandleFunc("DELETE /api/admin/proxies/{id}", authz(s.handleAdminProxyOne))
	mux.HandleFunc("POST /api/admin/proxies/{id}/probe", authz(s.handleAdminProxyProbe))

	mux.HandleFunc("GET /api/admin/tasks", authz(s.handleAdminTasks))
	mux.HandleFunc("GET /api/admin/tasks/events", authz(s.handleAdminTaskEventsAll))
	mux.HandleFunc("GET /api/admin/tasks/{id}", authz(s.handleAdminTaskOne))
	mux.HandleFunc("GET /api/admin/tasks/{id}/events", authz(s.handleAdminTaskEvents))
	mux.HandleFunc("POST /api/admin/tasks/{id}/cancel", authz(s.handleAdminTaskCancel))

	mux.HandleFunc("GET /api/admin/emulation", authz(s.handleAdminEmulation))
	mux.HandleFunc("PUT /api/admin/emulation", authz(s.handleAdminEmulation))
	mux.HandleFunc("POST /api/admin/emulation/websearch/test", authz(s.handleAdminEmulationSearchTest))
}

func (s *Server) websiteURL() string {
	if s.runtime != nil {
		if v, ok := s.runtime.Get()["website_url"].(string); ok && v != "" {
			return v
		}
	}
	if s.cfg != nil && s.cfg.WebsiteURL != "" {
		return s.cfg.WebsiteURL
	}
	return "https://example.com"
}
