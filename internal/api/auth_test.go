package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"prism-2api/internal/admin"
	"prism-2api/internal/clientkeys"
	"prism-2api/internal/config"
)

func newAuthTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	rc, err := admin.LoadOrCreate(filepath.Join(dir, "config.json"),
		"https://api2.cursor.sh", "https://cursor.com", "sk-master", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	keys := clientkeys.New()
	if k := rc.PublicAPIKey(); k != "" {
		keys.EnsureSystemKey("系统密钥", "", k)
	}
	return &Server{
		cfg:     &config.Config{APIKeyAuth: "sk-master", CredentialDir: dir},
		runtime: rc,
		sess:    admin.NewSessionManager(),
		logs:    admin.NewLogStore(32),
		keys:    keys,
	}
}

func TestAdminAuthRejectsBearerAPIKey(t *testing.T) {
	s := newAuthTestServer(t)
	h := s.adminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer api_key_auth must 401, got %d %s", w.Code, w.Body.String())
	}
}

func TestAdminAuthAcceptsSessionCookie(t *testing.T) {
	s := newAuthTestServer(t)
	if err := s.runtime.ChangePassword("s3cret-ok"); err != nil {
		t.Fatal(err)
	}
	h := s.adminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	req.AddCookie(s.sess.SessionCookie(s.sess.Issue("admin")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session cookie must 200, got %d %s", w.Code, w.Body.String())
	}
}

func TestDefaultPasswordLoginMustChange(t *testing.T) {
	s := newAuthTestServer(t)
	h := s.Handler()

	login := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"admin","password":"admin123"}`))
	login.Header.Set("Content-Type", "application/json")
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, login)
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d %s", lw.Code, lw.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(lw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["must_change_password"] != true {
		t.Fatalf("login must_change_password: %v", body)
	}
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Name == s.sess.CookieName() {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing session cookie")
	}

	cfgReq := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	cfgReq.AddCookie(cookie)
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, cfgReq)
	if cw.Code != http.StatusForbidden {
		t.Fatalf("other admin route must 403 until password change, got %d %s", cw.Code, cw.Body.String())
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/admin/me", nil)
	meReq.AddCookie(cookie)
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, meReq)
	if mw.Code != http.StatusOK {
		t.Fatalf("me must be allowed during must_change, got %d %s", mw.Code, mw.Body.String())
	}

	pwReq := httptest.NewRequest(http.MethodPut, "/api/admin/password", strings.NewReader(`{"password":"n3w-pass"}`))
	pwReq.Header.Set("Content-Type", "application/json")
	pwReq.AddCookie(cookie)
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, pwReq)
	if pw.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", pw.Code, pw.Body.String())
	}

	cfgReq2 := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	cfgReq2.AddCookie(cookie)
	cw2 := httptest.NewRecorder()
	h.ServeHTTP(cw2, cfgReq2)
	if cw2.Code != http.StatusOK {
		t.Fatalf("after change config must 200, got %d %s", cw2.Code, cw2.Body.String())
	}
}

func TestRequireAPIKeyAcceptsClientKey(t *testing.T) {
	s := newAuthTestServer(t)
	ck := s.keys.Create("probe", "", "team-a")
	h := s.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok || id.KeyID != ck.ID || id.Group != "team-a" {
			t.Errorf("identity: %+v ok=%v", id, ok)
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+ck.Key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("csk_* must 200, got %d %s", w.Code, w.Body.String())
	}
}
