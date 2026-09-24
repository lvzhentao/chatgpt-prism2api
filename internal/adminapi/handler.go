package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/auth"
	"prism-2api/internal/groups"
	"prism-2api/internal/pool"
)

// Handler 管理 JSON 处理器。Keys/Groups 可为 nil（未接线时 503）。
type Handler struct {
	Pool               *pool.Pool
	Logs               *admin.LogStore
	Keys               ClientKeyStore
	Groups             *groups.Store
	Started            time.Time
	Keepalive          *auth.Keepalive
	WebsiteURL         string
	AfterAccountChange func(acc *pool.Account)
	BatchConcurrency   func() int
	importMu           sync.Mutex
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeNotReady(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// Dashboard GET /api/admin/dashboard
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := Dashboard{
		UptimeSeconds: int(time.Since(h.Started).Seconds()),
		Cooldowns:     []CooldownItem{},
		Stats: DashboardStats{
			ByAccount: map[string]int{},
			ByModel:   map[string]int{},
			ByPath:    map[string]int{},
		},
	}
	if h.Keepalive != nil {
		out.RefreshFailures = h.Keepalive.FailureCounts()
	}
	if h.Pool != nil {
		snaps := h.Pool.List()
		out.Accounts.Total = len(snaps)
		now := time.Now().Unix()
		for _, s := range snaps {
			if s.Enabled {
				out.Accounts.Enabled++
			} else {
				out.Accounts.Disabled++
			}
			cooling := s.CooldownUntil > now
			if cooling {
				out.Accounts.Cooling++
				out.Cooldowns = append(out.Cooldowns, CooldownItem{
					Name:      s.Name,
					Reason:    s.CooldownReason,
					FailClass: s.FailClass,
					Until:     s.CooldownUntil,
				})
			}
			if s.Enabled && !cooling && s.LoggedIn {
				out.Accounts.Ready++
			}
		}
	}
	if h.Logs != nil {
		st := h.Logs.ComputeStats()
		out.Stats = DashboardStats{
			TotalRequests:   st.TotalRequests,
			ErrorRequests:   st.ErrorRequests,
			ErrorRate:       st.ErrorRate,
			TotalPrompt:     st.TotalPrompt,
			TotalCompletion: st.TotalCompletion,
			TotalTokens:     st.TotalTokens,
			AvgLatencyMs:    st.AvgLatency,
			ByAccount:       st.ByAccount,
			ByModel:         st.ByModel,
			ByPath:          st.ByPath,
			ByFailClass:     st.ByFailClass,
			ByClientKey:     st.ByClientKey,
			TokensByAccount: st.TokensByAccount,
			TokensByModel:   st.TokensByModel,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Usage GET /api/admin/usage
// ?refresh=1 时对照 cockpit 打 cursor.com/api/usage-summary（及 GetUserMeta / Stripe）。
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1" || r.Method == http.MethodPost
	resp := UsageResponse{Accounts: []AccountUsage{}}
	if h.Pool != nil {
		accounts := h.Pool.Accounts()
		if refresh {
			refreshAccountUsages(accounts, h.websiteURL(), h.concurrency())
		}
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			v := acc.UsageView()
			resp.Accounts = append(resp.Accounts, AccountUsage{
				Name:         acc.Name,
				SuccessCount: v.SuccessCount,
				Vendor:       v.Vendor,
				UsageError:   v.Error,
				UsageAt:      v.UpdatedAt,
			})
			resp.TotalSuccess += v.SuccessCount
		}
	}
	if h.Keys != nil {
		keys := h.Keys.List()
		ku := &KeyUsage{Count: len(keys)}
		for _, k := range keys {
			ku.TotalCalls += k.TotalCalls
			ku.TotalTokens += k.TotalTokens
		}
		resp.Keys = ku
	}
	writeJSON(w, http.StatusOK, resp)
}

// RefreshAccountUsage POST /api/admin/accounts/{name}/usage/refresh
func (h *Handler) RefreshAccountUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Pool == nil {
		writeNotReady(w)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	acc := h.Pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	v := acc.RefreshUsage(h.websiteURL())
	writeJSON(w, http.StatusOK, AccountUsage{
		Name:         acc.Name,
		SuccessCount: v.SuccessCount,
		Vendor:       v.Vendor,
		UsageError:   v.Error,
		UsageAt:      v.UpdatedAt,
	})
}

func (h *Handler) websiteURL() string {
	if h != nil && strings.TrimSpace(h.WebsiteURL) != "" {
		return h.WebsiteURL
	}
	return "https://example.com"
}

func refreshAccountUsages(accounts []*pool.Account, websiteURL string, concurrency int) {
	n := concurrency
	if n < 1 {
		n = 4
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a *pool.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			a.RefreshUsage(websiteURL)
		}(acc)
	}
	wg.Wait()
}

// Events GET /api/admin/events
// Accept: text/event-stream 或 ?stream=1 → SSE（订阅新日志 / 冷却 / 登录 + 心跳）；否则 JSON 最近 N 条。
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stream := r.URL.Query().Get("stream") == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if stream {
		h.streamEvents(w, r)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	var entries []admin.LogEntry
	if h.Logs != nil {
		entries = h.Logs.Query(limit, "", "", "", "")
	}
	if entries == nil {
		entries = []admin.LogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": entries, "count": len(entries)})
}

func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	var ch <-chan admin.Event
	unsub := func() {}
	if h.Logs != nil {
		ch, unsub = h.Logs.Subscribe()
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			typ := ev.Type
			if typ == "" {
				typ = admin.EventLog
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
			flusher.Flush()
		}
	}
}

// PatchAccounts PATCH /api/admin/accounts
func (h *Handler) PatchAccounts(w http.ResponseWriter, r *http.Request) {
	if h.Pool == nil {
		writeNotReady(w)
		return
	}
	var body AccountPatch
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	acc := h.Pool.Get(body.Name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	acc.ApplyAdminPatch(body.toPool())
	h.touchAccount(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Snapshot()})
}

func (h *Handler) touchAccount(acc *pool.Account) {
	if acc == nil {
		return
	}
	if h.AfterAccountChange != nil {
		h.AfterAccountChange(acc)
		return
	}
	if h.Pool != nil {
		acc.ApplyClientProxy(h.Pool.GlobalProxy())
	}
}

func (h *Handler) concurrency() int {
	if h.BatchConcurrency != nil {
		if n := h.BatchConcurrency(); n > 0 {
			return n
		}
	}
	return 4
}

// ImportItemError 单条导入失败。
type ImportItemError struct {
	Name  string `json:"name,omitempty"`
	Error string `json:"error"`
}

// ImportResult 批量导入汇总。
type ImportResult struct {
	Imported int               `json:"imported"`
	Skipped  int               `json:"skipped"`
	Errors   []ImportItemError `json:"errors"`
}

// ImportOne 导入或覆盖单个账号。
func (h *Handler) ImportOne(in AccountImport, overwrite bool) (imported, skipped bool, err error) {
	if h.Pool == nil {
		return false, false, fmt.Errorf("not ready")
	}
	h.importMu.Lock()
	defer h.importMu.Unlock()
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = pool.SanitizeName(in.Email)
	}
	if name == "" {
		return false, false, fmt.Errorf("name is required")
	}
	if !pool.ValidName(name) {
		return false, false, fmt.Errorf("invalid account name %q (allowed: [a-zA-Z0-9_-], 1-32 chars)", name)
	}
	apiKey := strings.TrimSpace(in.APIKey)
	access, _ := pickTokens(in.AccessToken)
	if access == "" {
		access, _ = pickTokens(in.SessionToken)
	}
	if access != "" {
		if exp := auth.JWTExpiry(access); exp > 0 && exp < time.Now().Unix() {
			return false, false, fmt.Errorf("access token expired")
		}
	}
	existing := h.Pool.Get(name)
	if existing != nil && !overwrite {
		return false, true, nil
	}
	if dup := h.Pool.FindDuplicate(name, in.Email, "", access); dup != nil {
		if overwrite {
			return false, false, fmt.Errorf("duplicate of %q", dup.Name)
		}
		return false, true, nil
	}
	if existing == nil && apiKey == "" && access == "" && strings.TrimSpace(in.RefreshToken) == "" {
		return false, false, fmt.Errorf("api_key / access_token / refresh bundle is required for new account")
	}
	if apiKey != "" {
		if addErr := h.Pool.AddAPIKey(name, apiKey); addErr != nil {
			return false, false, addErr
		}
	}
	if access != "" || strings.TrimSpace(in.RefreshToken) != "" {
		// 只有 refresh（凭据束）时也写入：这是「导入账号密码 + 2FA，等着被选中去登录」的形态，
		// 账号此刻没有可用的 access token（管理台显示未登录），登录成功后才会被启用。
		tok := &auth.Token{
			AccessToken:  access,
			RefreshToken: strings.TrimSpace(in.RefreshToken),
			APIKey:       apiKey,
		}
		if setErr := h.Pool.SetToken(name, tok); setErr != nil {
			return false, false, setErr
		}
	}
	existing = h.Pool.Get(name)
	if existing == nil {
		return false, false, fmt.Errorf("account missing after import")
	}
	if email := strings.TrimSpace(in.Email); email != "" {
		snap := existing.UsageView().Vendor
		if snap.Email == "" {
			snap.Email = email
			existing.RememberUsage(snap, "")
		}
	}
	existing.ApplyAdminPatch(in.toPool())
	h.touchAccount(existing)
	return true, false, nil
}

// ImportAccounts POST /api/admin/accounts/import
func (h *Handler) ImportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Pool == nil {
		writeNotReady(w)
		return
	}
	var body struct {
		Accounts  json.RawMessage `json:"accounts"`
		Text      string          `json:"text"`
		Overwrite bool            `json:"overwrite"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	accounts, err := ParseImportRequest(body.Accounts, body.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res := ImportResult{Errors: []ImportItemError{}}
	for _, in := range accounts {
		imported, skipped, err := h.ImportOne(in, body.Overwrite)
		if err != nil {
			res.Errors = append(res.Errors, ImportItemError{Name: strings.TrimSpace(in.Name), Error: err.Error()})
			continue
		}
		if skipped {
			res.Skipped++
			continue
		}
		if imported {
			res.Imported++
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// ExportAccounts GET /api/admin/accounts/export — 元数据，不含完整令牌。
func (h *Handler) ExportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []AccountExport{}, "total": 0})
		return
	}
	snaps := h.Pool.List()
	out := make([]AccountExport, 0, len(snaps))
	for _, s := range snaps {
		item := AccountExport{
			Name:           s.Name,
			ID:             s.ID,
			Enabled:        s.Enabled,
			Priority:       s.Priority,
			Weight:         s.Weight,
			Groups:         s.Groups,
			ProxyURL:       maskProxy(s.ProxyURL),
			RPM:            s.RPM,
			DailyMax:       s.DailyMax,
			HasAPIKey:      s.HasAPIKey,
			ExpiresAt:      s.ExpiresAt,
			SuccessCount:   s.SuccessCount,
			CooldownReason: s.CooldownReason,
			CooldownUntil:  s.CooldownUntil,
		}
		if acc := h.Pool.Get(s.Name); acc != nil && acc.Tokens != nil {
			item.HasRefreshToken = acc.Tokens.HasRefreshToken()
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "total": len(out)})
}

func maskProxy(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid)"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}
