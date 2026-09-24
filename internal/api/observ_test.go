package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPprofLocalhostOnly(t *testing.T) {
	s := setupTestServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("remote pprof: got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "127.0.0.1:9999"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("localhost pprof: got %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "[::1]:9999"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ipv6 pprof: got %d", w.Code)
	}
}

func TestLogMiddlewareRedactsSecretsAndSetsRequestID(t *testing.T) {
	s := setupTestServer(t)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"admin","password":"admin123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer super-secret-token")
	req.Header.Set("X-Request-ID", "req-fixed-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Header().Get("X-Request-ID") != "req-fixed-1" {
		t.Fatalf("request id echo: %q", w.Header().Get("X-Request-ID"))
	}

	entries := s.logs.Query(10, "", "", "", "/api/admin/login")
	if len(entries) == 0 {
		t.Fatal("expected login log")
	}
	e := entries[0]
	if e.RequestID != "req-fixed-1" {
		t.Fatalf("request_id: %q", e.RequestID)
	}
	if strings.Contains(e.RequestBody, "admin123") {
		t.Fatalf("password leaked in body: %s", e.RequestBody)
	}
	if !strings.Contains(e.RequestBody, "[REDACTED]") {
		t.Fatalf("expected redacted password: %s", e.RequestBody)
	}
	if auth := e.Headers["authorization"]; strings.Contains(auth, "super-secret-token") {
		t.Fatalf("authorization leaked: %q", auth)
	}
}

func TestRecordClientUsage(t *testing.T) {
	s := setupTestServer(t)
	k := s.keys.Create("usage-test", "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIdentity(req.Context(), Identity{KeyID: k.ID}))
	s.recordClientUsage(req, 11, 7)
	got, ok := s.keys.Get(k.ID)
	if !ok {
		t.Fatal("key missing")
	}
	if got.TotalInputTokens != 11 || got.TotalOutputTokens != 7 {
		t.Fatalf("usage %+v", got)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	if !isLoopbackAddr("127.0.0.1:80") || !isLoopbackAddr("[::1]:1") {
		t.Fatal("expected loopback")
	}
	if isLoopbackAddr("10.0.0.1:80") || isLoopbackAddr("") {
		t.Fatal("expected non-loopback")
	}
}

func TestBodyCaptureKeepsRequestTail(t *testing.T) {
	s := setupTestServer(t)
	h := s.Handler()
	marker := "UNIQUE_TAIL_PROBE_CCTEST"
	body := `{"model":"claude-opus-4-8","system":"` + strings.Repeat("S", 40000) + `","messages":[{"role":"user","content":"` + marker + `"}],"max_tokens":16}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	entries := s.logs.Query(10, "", "", "", "/v1/messages")
	if len(entries) == 0 {
		t.Fatal("expected messages log")
	}
	if !strings.Contains(entries[0].RequestBody, marker) {
		t.Fatalf("tail marker missing from log: %s", entries[0].RequestBody[max(0, len(entries[0].RequestBody)-200):])
	}
}

func TestRemoteAdminBlockedWhenListeningAllInterfaces(t *testing.T) {
	s := setupTestServer(t)
	if _, _, err := s.runtime.Update(map[string]any{"listen_addr": "0.0.0.0:8080", "allow_remote_admin": false}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{}`))
	req.RemoteAddr = "8.8.8.8:9"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote admin: got %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/config/schema", nil)
	req.RemoteAddr = "8.8.8.8:9"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("schema remote: got %d", w.Code)
	}
}

func TestApplyHistoryCompressTiers(t *testing.T) {
	t.Cleanup(func() { ApplyHistoryCompress("standard") })
	ApplyHistoryCompress("off")
	if toolResultMaxChars < 1<<20 {
		t.Fatalf("off tier still truncating: %d", toolResultMaxChars)
	}
	ApplyHistoryCompress("aggressive")
	if historyMaxChars != 40000 {
		t.Fatalf("aggressive history %d", historyMaxChars)
	}
}
