package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 服务器默认配置下不得有任何本地并发闸门：启动后新增的账号也是无上限的。
func TestServerHasNoLocalConcurrencyGate(t *testing.T) {
	s := setupTestServer(t)
	if err := s.pool.AddAPIKey("acc1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := s.pool.Get("acc1")
	if _, limit, _ := acc.ConcurrencySnapshot(); limit != 0 {
		t.Fatalf("默认上限应为 0（不限制），got %d", limit)
	}
	for i := 0; i < 128; i++ {
		if !acc.Acquire() {
			t.Fatalf("第 %d 路并发必须放行（本地不设限）", i+1)
		}
	}
	for i := 0; i < 128; i++ {
		acc.Release()
	}
}

// 管理端显式配了上限时，启动后新增的账号也必须跟上（不得静默忽略）。
func TestServerConcurrencyLimitAppliesToNewAccounts(t *testing.T) {
	s := setupTestServer(t)
	if _, _, err := s.runtime.Update(map[string]any{"account_concurrency": 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.AddAPIKey("acc2", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := s.pool.Get("acc2")
	if _, limit, _ := acc.ConcurrencySnapshot(); limit != 3 {
		t.Fatalf("新账号上限应跟随配置 3，got %d", limit)
	}
	for i := 0; i < 3; i++ {
		if !acc.Acquire() {
			t.Fatalf("第 %d 路应放行", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("配了 3 之后第 4 路必须被拦")
	}
	for i := 0; i < 3; i++ {
		acc.Release()
	}
}

// 管理台改别的配置时，不得把后端地址回灌成库里存的旧值：
// 池子启动时的生效地址来自 env / compose，库里存的可能是历史占位值（线上实测 api.example.com）。
func TestConfigPutDoesNotClobberBackendURL(t *testing.T) {
	s := setupTestServer(t)
	if err := s.runtime.ChangePassword("test-admin-pass"); err != nil {
		t.Fatal(err)
	}
	tok := s.sess.Issue("admin")
	// 制造"库里存的值 ≠ 启动生效值"的局面：池子指向启动地址，配置里是另一个旧值。
	s.pool.SetBaseURL("http://boot-value.invalid")
	if _, _, err := s.runtime.Update(map[string]any{"api_base_url": "https://stale.example.com"}); err != nil {
		t.Fatal(err)
	}

	put := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/config", strings.NewReader(body))
		req.AddCookie(s.sess.SessionCookie(tok))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s → %d %s", body, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}

	put(`{"account_concurrency":1}`)
	if got := s.pool.BaseURL(); got != "http://boot-value.invalid" {
		t.Fatalf("没提交 api_base_url 却改了后端地址：%q（应保持启动值）", got)
	}

	put(`{"api_base_url":"https://prism.openai.com"}`)
	if got := s.pool.BaseURL(); got != "https://prism.openai.com" {
		t.Fatalf("显式提交 api_base_url 应即时生效，got %q", got)
	}
}
