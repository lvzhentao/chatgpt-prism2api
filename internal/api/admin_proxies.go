package api

import (
	"net/http"

	"prism-2api/internal/egress"
	"prism-2api/internal/proxypool"
	"prism-2api/internal/tasks"
)

func (s *Server) handleAdminProxies(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not ready"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := s.proxies.List()
		if list == nil {
			list = []proxypool.Proxy{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"proxies": list, "total": len(list)})
	case http.MethodPost:
		var req proxypool.CreateRequest
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
			return
		}
		p, err := s.proxies.Create(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		s.pool.RefreshEgress()
		writeJSON(w, http.StatusOK, p)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) handleAdminProxyOne(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "not ready"})
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		p, ok := s.proxies.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "proxy not found"})
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPatch, http.MethodPut:
		var req proxypool.PatchRequest
		if err := decodeJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
			return
		}
		p, err := s.proxies.Patch(id, req)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "proxy not found" {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		s.pool.RefreshEgress()
		writeJSON(w, http.StatusOK, p)
	case http.MethodDelete:
		if err := s.proxies.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		s.pool.RefreshEgress()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) handleAdminProxyProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	id := r.PathValue("id")
	p, ok := s.proxies.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "proxy not found"})
		return
	}
	t := s.jobs.Enqueue(tasks.CreateRequest{
		Type:       "proxy.probe",
		Title:      "探测出口 " + p.Name,
		Total:      1,
		Cancelable: true,
		Meta:       map[string]any{"proxy_id": id},
		Run: func(ctx *tasks.Context) (any, error) {
			ctx.Info("开始探测 " + p.Name + " (" + p.Kind + ")")
			ip, err := egress.ProbeEgressIP(proxypool.SettingsFor(p, "probe-"+id))
			if err != nil {
				_, _ = s.proxies.RememberProbe(id, "", err.Error())
				return nil, err
			}
			updated, _ := s.proxies.RememberProbe(id, ip, "")
			ctx.Progress(1, 1, "出口 IP "+ip)
			ctx.Info("探测成功，出口 IP " + ip)
			return updated, nil
		},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}
