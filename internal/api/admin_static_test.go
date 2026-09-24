package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"prism-2api/internal/config"
)

func TestMountAdminStaticSPA(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, "assets")
	if err := os.Mkdir(asset, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: &config.Config{AdminStaticDir: dir}}
	mux := http.NewServeMux()
	s.mountAdminStatic(mux)

	redirect := httptest.NewRecorder()
	mux.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/", nil))
	if redirect.Code != http.StatusFound || redirect.Header().Get("Location") != "/admin/" {
		t.Fatalf("root redirect: code=%d loc=%s", redirect.Code, redirect.Header().Get("Location"))
	}

	idx := httptest.NewRecorder()
	mux.ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if idx.Code != http.StatusOK || idx.Body.String() != "<html>ok</html>" {
		t.Fatalf("spa fallback: code=%d body=%q", idx.Code, idx.Body.String())
	}

	js := httptest.NewRecorder()
	mux.ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil))
	if js.Code != http.StatusOK || js.Body.String() != "console.log(1)" {
		t.Fatalf("asset: code=%d body=%q", js.Code, js.Body.String())
	}
}
