package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"prism-2api/internal/adapter"
	"prism-2api/internal/admin"
)

func (s *Server) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	user := ""
	if c, err := r.Cookie(s.sess.CookieName()); err == nil {
		if u, ok := s.sess.Verify(c.Value); ok {
			user = u
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"username":             user,
		"must_change_password": s.runtime != nil && s.runtime.MustChange(),
	})
}

func (s *Server) handleAdminAccountGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	name := r.PathValue("name")
	acc := s.pool.Get(name)
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("log_limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	logs := []admin.LogEntry{}
	if s.logs != nil {
		logs = s.logs.Query(limit, "", name, "", "")
		if logs == nil {
			logs = []admin.LogEntry{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": acc.Snapshot(),
		"usage":   acc.UsageView(),
		"logs":    logs,
		"egress":  s.resolveAccountEgress(acc),
	})
}

func (s *Server) handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	name := r.PathValue("name")
	if s.pool.Get(name) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found"})
		return
	}
	if err := s.pool.Remove(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
}

func (s *Server) handleAdminLogGet(w http.ResponseWriter, r *http.Request) {
	idRaw := r.PathValue("id")
	if strings.HasPrefix(idRaw, "req_") || strings.Contains(idRaw, "-") {
		e := s.logs.GetByRequestID(strings.TrimPrefix(idRaw, "req_"))
		if e == nil {
			e = s.logs.GetByRequestID(idRaw)
		}
		if e == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "log not found"})
			return
		}
		writeJSON(w, http.StatusOK, e)
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	e := s.logs.Get(id)
	if e == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "log not found"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "total": 0})
		return
	}
	var infos []*adapter.ModelInfo
	var err error
	if r.Method == http.MethodPost || r.URL.Query().Get("refresh") == "1" {
		infos, err = s.catalog.Refresh()
	} else {
		infos, err = s.catalog.Load()
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "models": []any{}})
		return
	}
	type row struct {
		ID                string   `json:"id"`
		DisplayName       string   `json:"display_name"`
		ServerModelName   string   `json:"server_model_name"`
		Aliases           []string `json:"aliases,omitempty"`
		SupportsThinking  bool     `json:"supports_thinking"`
		SupportsImages    bool     `json:"supports_images"`
		SupportsAgent     bool     `json:"supports_agent"`
		ContextTokenLimit int32    `json:"context_token_limit,omitempty"`
		ThinkingLevel     string   `json:"thinking_level,omitempty"`
		MaxMode           bool     `json:"max_mode,omitempty"`
	}
	out := make([]row, 0, len(infos))
	for _, m := range infos {
		if m == nil {
			continue
		}
		out = append(out, row{
			ID: adapter.OfficializeModelID(m.ID), DisplayName: m.DisplayName, ServerModelName: m.ServerModelName,
			Aliases: m.Aliases, SupportsThinking: m.SupportsThinking, SupportsImages: m.SupportsImages,
			SupportsAgent: m.SupportsAgent, ContextTokenLimit: m.ContextTokenLimit,
			ThinkingLevel: m.ThinkingLevel, MaxMode: m.MaxMode,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "total": len(out)})
}

func (s *Server) handleAdminBatches(w http.ResponseWriter, r *http.Request) {
	if s.batchStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"batches": []any{}, "total": 0})
		return
	}
	list := s.batchStore.list()
	out := make([]batchResponse, 0, len(list))
	for _, b := range list {
		out = append(out, batchToResponse(b, "/v1/messages/batches/"+b.id+"/results"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": out, "total": len(out)})
}

func (s *Server) handleAdminBatchGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.batchStore == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	b := s.batchStore.get(id)
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "batch not found"})
		return
	}
	writeJSON(w, http.StatusOK, batchToResponse(b, "/v1/messages/batches/"+b.id+"/results"))
}

func decodeJSON(r *http.Request, dst any) error {
	return json.NewDecoder(ioLimit(r.Body)).Decode(dst)
}
