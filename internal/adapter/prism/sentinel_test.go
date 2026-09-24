package prism

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mintSentinelToken 的 NDJSON 事件解析：done.token 必须原样取出，error 事件要变错误。
func TestMintSentinelTokenParsesEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			t.Errorf("请求应打 /login，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte("{\"event\":\"state\",\"name\":\"sentinel_assets\"}\n" +
			"{\"event\":\"state\",\"name\":\"sentinel_proof\"}\n" +
			"{\"event\":\"done\",\"token\":\"{\\\"p\\\":\\\"x\\\",\\\"c\\\":\\\"y\\\"}\"}\n"))
	}))
	defer srv.Close()
	t.Setenv("PRISM_LOGIN_SIDECAR_URL", srv.URL)
	tok, err := mintSentinelToken(context.Background())
	if err != nil {
		t.Fatalf("应成功取 token: %v", err)
	}
	if !strings.Contains(tok, `"p":"x"`) {
		t.Fatalf("token 原文应保留: %q", tok)
	}
}

func TestMintSentinelTokenErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{\"event\":\"error\",\"stage\":\"sentinel_token\",\"message\":\"runner 未产出 token\"}\n"))
	}))
	defer srv.Close()
	t.Setenv("PRISM_LOGIN_SIDECAR_URL", srv.URL)
	if _, err := mintSentinelToken(context.Background()); err == nil || !strings.Contains(err.Error(), "runner 未产出") {
		t.Fatalf("error 事件应转成错误: %v", err)
	}
}

// sentinelEnabled：WEB2API_SENTINEL=off 显式关闭优先于 sidecar 配置。
func TestSentinelEnabledSwitch(t *testing.T) {
	t.Setenv("PRISM_LOGIN_SIDECAR_URL", "http://sidecar:8099")
	t.Setenv("WEB2API_SENTINEL", "off")
	if sentinelEnabled() {
		t.Fatal("off 必须关闭 sentinel")
	}
	t.Setenv("WEB2API_SENTINEL", "")
	if !sentinelEnabled() {
		t.Fatal("配置了 sidecar 且未显式关闭时应启用")
	}
	t.Setenv("PRISM_LOGIN_SIDECAR_URL", "")
	if sentinelEnabled() {
		t.Fatal("无 sidecar 时不可启用（本地无铸造设施）")
	}
}
