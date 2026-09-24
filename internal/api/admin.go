package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/adminapi"
	"prism-2api/internal/pool"
)

// adminAuth 管理接口鉴权：仅接受登录会话 Cookie（c2a_session）。
// 公开 API Key（Bearer / x-api-key）一律 401。MASTER ≠ PUBLIC。
// 默认密码未改时，除改密外的管理 API 返回 403。
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminRemoteAllowed(r) {
			s.rejectRemoteAdmin(w)
			return
		}
		c, err := r.Cookie(s.sess.CookieName())
		if err != nil || c.Value == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{"message": "unauthorized: 请先登录管理后台", "type": "auth_error"},
			})
			return
		}
		if _, ok := s.sess.Verify(c.Value); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{"message": "unauthorized: 请先登录管理后台", "type": "auth_error"},
			})
			return
		}
		if s.runtime != nil && s.runtime.MustChange() && !adminAllowDuringMustChange(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":                map[string]string{"message": "must change default password", "type": "must_change_password"},
				"must_change_password": true,
			})
			return
		}
		next(w, r)
	}
}

func adminAllowDuringMustChange(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/admin/password", "/api/admin/me", "/api/admin/logout":
		return true
	default:
		return false
	}
}

// handleAdminLogin 管理员登录（POST /api/admin/login，body {username,password}）。
// 成功：设置 HttpOnly 会话 Cookie 并返回 {ok:true}。
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.adminRemoteAllowed(r) {
		s.rejectRemoteAdmin(w)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	cfg := s.runtime.Get()
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "用户名和密码不能为空", "type": "auth_error"}})
		return
	}
	if req.Username != cfg["admin_username"].(string) || !s.runtime.CheckPassword(req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "用户名或密码错误", "type": "auth_error"}})
		return
	}
	tok := s.sess.Issue(req.Username)
	http.SetCookie(w, s.sess.SessionCookie(tok))
	if s.logs != nil {
		s.logs.Publish(admin.Event{Type: admin.EventLogin, Time: time.Now(), User: req.Username})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"username":             req.Username,
		"must_change_password": s.runtime.MustChange(),
	})
}

// handleAdminPassword 改管理员密码（PUT /api/admin/password）。拒绝 admin123；成功后清除 must_change_password。
func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	pw := req.NewPassword
	if pw == "" {
		pw = req.Password
	}
	if err := s.runtime.ChangePassword(pw); err != nil {
		status := http.StatusBadRequest
		if !errors.Is(err, admin.ErrDefaultAdminPassword) {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "auth_error"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "must_change_password": false})
}

// handleAdminLogout 退出登录（POST /api/admin/logout，清除会话 Cookie）。
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	http.SetCookie(w, s.sess.ClearCookie())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminConfig 读取/修改运行时配置。
// GET /api/admin/config → 当前配置
// PUT /api/admin/config → 更新（body 为部分字段 JSON）
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.runtime.Get())
	case http.MethodPut:
		var patch map[string]any
		if err := json.NewDecoder(ioLimit(r.Body)).Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid body: " + err.Error()}})
			return
		}
		cur, restart, err := s.runtime.Update(patch)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, admin.ErrDefaultAdminPassword) {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		// 即时生效的联动：只处理本次请求真的提交了键的字段。
		// 不能看合并后的 cur——库里存的 api_base_url 可能是编译期占位值
		// （https://api.example.com），而启动时的生效值是 env / compose 给的，
		// 照 cur 回灌会把后端地址改坏，之后每次建会话都打错域名。
		if _, sent := patch["api_base_url"]; sent {
			if v, ok := cur["api_base_url"].(string); ok && v != "" {
				s.pool.SetBaseURL(v)
				s.catalog.Invalidate()
				log.Printf("admin: api_base_url -> %s (pool + catalog updated)", v)
			}
		}
		if _, sent := patch["cache_ttl_seconds"]; sent {
			if v, ok := cur["cache_ttl_seconds"].(int); ok && v > 0 {
				s.catalog.SetCacheTTL(time.Duration(v) * time.Second)
			}
		}
		if _, sent := patch["log_max_entries"]; sent {
			if v, ok := cur["log_max_entries"].(int); ok && v > 0 {
				s.logs.SetMax(v)
			}
		}
		if s.sched != nil {
			s.sched.SetConfig(schedulerFromRuntime(s.runtime))
		}
		s.ensureSystemKey()
		s.applyRuntimeSideEffects()
		writeJSON(w, http.StatusOK, map[string]any{"config": cur, "restart_required": restart})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"message": "use GET or PUT"}})
	}
}

// handleAdminLogs 查询请求日志。
// GET /api/admin/logs?limit=&offset=&model=&account=&status=&path=&fail_class=
// DELETE /api/admin/logs → 清空
func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.logs.Clear()
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
		return
	}
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	entries := s.logs.QueryFilter(limit+offset, admin.LogFilter{
		Model:     q.Get("model"),
		Account:   q.Get("account"),
		Status:    q.Get("status"),
		Path:      q.Get("path"),
		FailClass: q.Get("fail_class"),
	})
	if offset > len(entries) {
		offset = len(entries)
	}
	entries = entries[offset:]
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"count":   len(entries),
		"offset":  offset,
		"filter": map[string]string{
			"model": q.Get("model"), "account": q.Get("account"),
			"status": q.Get("status"), "path": q.Get("path"),
			"fail_class": q.Get("fail_class"),
		},
	})
}

// handleAdminStats 返回请求统计。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	st := s.logs.ComputeStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"total_requests":    st.TotalRequests,
		"error_requests":    st.ErrorRequests,
		"error_rate":        st.ErrorRate,
		"total_prompt":      st.TotalPrompt,
		"total_completion":  st.TotalCompletion,
		"total_tokens":      st.TotalTokens,
		"avg_latency_ms":    st.AvgLatency,
		"by_account":        st.ByAccount,
		"by_model":          st.ByModel,
		"by_path":           st.ByPath,
		"by_fail_class":     st.ByFailClass,
		"by_client_key":     st.ByClientKey,
		"tokens_by_account": st.TokensByAccount,
		"tokens_by_model":   st.TokensByModel,
		"accounts":          len(s.pool.List()),
		"uptime_seconds":    int(time.Since(s.started).Seconds()),
		"config": func() map[string]any {
			cfg := s.runtime.Get()
			if s.pg != nil {
				cfg["config_path"] = "postgres:documents"
			} else {
				cfg["config_path"] = "memory"
			}
			return cfg
		}(),
	})
}

// handleAdminAccounts 账号池管理（供管理端使用；与 /v1/accounts 行为一致）。
// GET 列表 / POST 新增（{name?, api_key?}）。
func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snaps := s.pool.List()
		if snaps == nil {
			snaps = []pool.Snapshot{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"accounts": snaps, "total": len(snaps)})
	case http.MethodPost:
		var body struct {
			Name         string `json:"name"`
			Email        string `json:"email"`
			APIKey       string `json:"api_key"`
			AccessToken  string `json:"access_token"`
			SessionToken string `json:"session_token"`
		}
		if err := json.NewDecoder(ioLimit(r.Body)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid body: " + err.Error()}})
			return
		}
		name := body.Name
		if name == "" {
			name = pool.SanitizeName(body.Email)
		}
		if name == "" {
			name = "default"
		}
		if body.APIKey != "" || body.AccessToken != "" || body.SessionToken != "" {
			h := &adminapi.Handler{Pool: s.pool, AfterAccountChange: s.pool.RefreshAccountEgress}
			if _, _, err := h.ImportOne(adminapi.AccountImport{
				Name:         name,
				Email:        body.Email,
				APIKey:       body.APIKey,
				AccessToken:  body.AccessToken,
				SessionToken: body.SessionToken,
			}, true); err != nil {
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
