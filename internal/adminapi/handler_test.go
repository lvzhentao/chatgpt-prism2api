package adminapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/auth"
	"prism-2api/internal/groups"
	"prism-2api/internal/persist"
	"prism-2api/internal/pool"
)

func TestDashboardEmptyPool(t *testing.T) {
	h := &Handler{Started: time.Now().Add(-5 * time.Second), Logs: admin.NewLogStore(10)}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil)
	w := httptest.NewRecorder()
	h.Dashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d %s", w.Code, w.Body.String())
	}
	var dash Dashboard
	if err := json.Unmarshal(w.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if dash.UptimeSeconds < 4 {
		t.Fatalf("uptime too small: %d", dash.UptimeSeconds)
	}
	if dash.Cooldowns == nil {
		t.Fatal("cooldowns should be empty slice not null")
	}
}

func TestGroupsCRUD(t *testing.T) {
	gs, err := groups.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Groups: gs}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/groups", strings.NewReader(`{"name":"team","description":"d"}`))
	w := httptest.NewRecorder()
	h.CreateGroup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
	w = httptest.NewRecorder()
	h.ListGroups(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"team"`) {
		t.Fatalf("list groups: %d %s", w.Code, w.Body.String())
	}
}

func TestClientKeysNotReady(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/client-keys", nil)
	w := httptest.NewRecorder()
	h.ListClientKeys(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"not ready"`) {
		t.Fatalf("body: %s", w.Body.String())
	}
}

func TestGroupsNotReady(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
	w := httptest.NewRecorder()
	h.ListGroups(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestPatchAccountAndExport(t *testing.T) {
	dir := t.TempDir()
	p, err := pool.New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("alpha", "sk-test"); err != nil {
		t.Fatal(err)
	}
	p.Get("alpha").Tokens.SetToken(&auth.Token{AccessToken: "aaa", APIKey: "sk-test"})
	h := &Handler{Pool: p, Started: time.Now()}

	body := `{"name":"alpha","priority":9,"weight":3,"enabled":true,"proxy_url":"http://user:secret@127.0.0.1:8080","rpm":60,"daily_max":1000,"groups":["team"]}`
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/accounts", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.PatchAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}

	snap := p.Get("alpha").Snapshot()
	if snap.Priority != 9 || snap.Weight != 3 || snap.RPM != 60 || snap.DailyMax != 1000 {
		t.Fatalf("patch not applied: %+v", snap)
	}
	if snap.ProxyURL != "http://user:secret@127.0.0.1:8080" {
		t.Fatalf("proxy: %q", snap.ProxyURL)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export", nil)
	w = httptest.NewRecorder()
	h.ExportAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if strings.Contains(raw, "secret") || strings.Contains(raw, "sk-test") {
		t.Fatalf("export leaked secret: %s", raw)
	}
	if !strings.Contains(raw, `"has_api_key":true`) {
		t.Fatalf("export missing has_api_key: %s", raw)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(`{"overwrite":true,"accounts":[{"name":"gamma","api_key":"sk-gamma","priority":2}]}`))
	w = httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if p.Get("gamma") == nil {
		t.Fatal("imported gamma missing")
	}
}

func TestImportSessionTokenWithoutAPIKey(t *testing.T) {
	dir := t.TempDir()
	p, err := pool.New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Pool: p}
	tok := importTestJWT(t)
	line := "Carol@outlook.com----pw----user_01C%3A%3A" + tok
	body, _ := json.Marshal(map[string]any{"text": line})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	acc := p.Get("Carol")
	if acc == nil {
		t.Fatal("imported Carol missing")
	}
	cur := acc.Tokens.Current()
	if cur == nil || cur.AccessToken != tok {
		t.Fatalf("token %+v", cur)
	}
	if acc.Snapshot().Email != "Carol@outlook.com" {
		t.Fatalf("email %q", acc.Snapshot().Email)
	}
}

func TestImportSkipsDuplicateIdentity(t *testing.T) {
	dir := t.TempDir()
	p, err := pool.New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Pool: p}
	tok := importTestJWT(t)
	first := "Alice@outlook.com----pw----user_01C%3A%3A" + tok
	body, _ := json.Marshal(map[string]any{"text": first})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	if p.Get("Alice") == nil {
		t.Fatal("Alice missing")
	}

	second := "Bob@outlook.com----pw----user_01C%3A%3A" + tok
	body, _ = json.Marshal(map[string]any{"text": second})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dup import: %d %s", w.Code, w.Body.String())
	}
	var res ImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Imported != 0 || res.Skipped != 1 {
		t.Fatalf("want skip duplicate, got %+v %s", res, w.Body.String())
	}
	if p.Get("Bob") != nil {
		t.Fatal("Bob should not be created")
	}

	other := importTestJWTWithSub(t, "auth0|user_01OTHER")
	sameEmail := "Alice@outlook.com----pw----user_01O%3A%3A" + other
	body, _ = json.Marshal(map[string]any{"text": sameEmail})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || p.Count() != 1 {
		t.Fatalf("same email should skip, got %+v count=%d", res, p.Count())
	}

	body, _ = json.Marshal(map[string]any{"overwrite": true, "text": second})
	req = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ImportAccounts(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0].Error, "duplicate of") {
		t.Fatalf("overwrite other name should error, got %+v", res)
	}
}

func TestUsageReturnsCachedWithoutRefresh(t *testing.T) {
	dir := t.TempDir()
	p, err := pool.New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("alpha", "sk-test"); err != nil {
		t.Fatal(err)
	}
	p.Get("alpha").Tokens.SetToken(&auth.Token{AccessToken: "aaa", APIKey: "sk-test"})
	p.Get("alpha").MarkSuccess()
	h := &Handler{Pool: p}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage", nil)
	w := httptest.NewRecorder()
	h.Usage(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("usage: %d %s", w.Code, w.Body.String())
	}
	var resp UsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalSuccess != 1 || len(resp.Accounts) != 1 || resp.Accounts[0].Name != "alpha" {
		t.Fatalf("usage %+v", resp)
	}
}

func TestEventsJSON(t *testing.T) {
	logs := admin.NewLogStore(10)
	logs.Add(admin.LogEntry{Path: "/v1/models", Status: 200, Time: time.Now()})
	h := &Handler{Logs: logs}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/events?limit=5", nil)
	w := httptest.NewRecorder()
	h.Events(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/v1/models") {
		t.Fatalf("expected log path, got %s", w.Body.String())
	}
}

func TestEventsSSESubscribe(t *testing.T) {
	logs := admin.NewLogStore(10)
	h := &Handler{Logs: logs}
	srv := httptest.NewServer(http.HandlerFunc(h.Events))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?stream=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	waitLine := func(substr string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case line := <-lines:
				if strings.Contains(line, substr) {
					return
				}
			case <-deadline:
				t.Fatalf("timeout waiting for %q", substr)
			}
		}
	}
	waitLine(": connected")
	logs.Add(admin.LogEntry{Path: "/v1/chat/completions", Status: 200, Time: time.Now(), FailClass: "rate_limit"})
	waitLine("event: log")
	waitLine("/v1/chat/completions")
	logs.Publish(admin.Event{Type: admin.EventCooldown, Account: "acc1", FailClass: "rate_limit"})
	waitLine("event: cooldown")
	cancel()
	io.Copy(io.Discard, resp.Body)
}

// 导入「只有凭据束（邮箱+密码+2FA）」的账号：必须能建、且默认不进调度
// （没有 access token = 未登录，等被选中去登录；内核 keepalive 会跳过停用账号）。
func TestImportBundleOnlyAccount(t *testing.T) {
	p, err := pool.Open(persist.NewMemory(), "https://x", "https://y", "", "web")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Pool: p}
	off := false
	_, _, err = h.ImportOne(AccountImport{
		Name:         "acct-a",
		Email:        "a@b.com",
		RefreshToken: `{"email":"a@b.com","password":"pw","totp_secret":"SECRET"}`,
		Enabled:      &off,
	}, false)
	if err != nil {
		t.Fatalf("导入只带凭据束的账号失败: %v", err)
	}
	acc := p.Get("acct-a")
	if acc == nil {
		t.Fatal("账号没建出来")
	}
	snap := acc.Snapshot()
	if snap.LoggedIn {
		t.Fatal("没有 access token 的账号不应显示为已登录")
	}
	if snap.Enabled {
		t.Fatal("导入的待登录账号不应处于启用态（会被保活自动登录）")
	}
	if got := tokenRefreshForTest(acc); got == "" {
		t.Fatal("凭据束没存进 RefreshToken，之后没法登录")
	}
}

func tokenRefreshForTest(a *pool.Account) string {
	if a == nil || a.Tokens == nil {
		return ""
	}
	if tok := a.Tokens.Current(); tok != nil {
		return tok.RefreshToken
	}
	return ""
}
