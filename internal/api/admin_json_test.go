package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-2api/internal/auth"
)

func TestAdminDashboardUnauthorized(t *testing.T) {
	s := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without cookie, got %d %s", w.Code, w.Body.String())
	}
}

func TestAdminDashboardAndPatchWithSession(t *testing.T) {
	s := setupTestServer(t)
	if err := s.pool.AddAPIKey("beta", "sk-beta"); err != nil {
		t.Fatal(err)
	}
	s.pool.Get("beta").Tokens.SetToken(&auth.Token{AccessToken: "aaa", APIKey: "sk-beta"})
	if err := s.runtime.ChangePassword("test-admin-pass"); err != nil {
		t.Fatal(err)
	}
	tok := s.sess.Issue("admin")
	handler := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil)
	req.AddCookie(s.sess.SessionCookie(tok))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	var dash map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if _, ok := dash["uptime_seconds"]; !ok {
		t.Fatalf("missing uptime_seconds: %s", w.Body.String())
	}

	body := `{"name":"beta","priority":4,"weight":2,"rpm":30}`
	req = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts", strings.NewReader(body))
	req.AddCookie(s.sess.SessionCookie(tok))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/client-keys", nil)
	req.AddCookie(s.sess.SessionCookie(tok))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("client-keys: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "masked_key") {
		t.Fatalf("expected masked_key in list: %s", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
	req.AddCookie(s.sess.SessionCookie(tok))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminEventsUnauthorized(t *testing.T) {
	s := setupTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/events", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
