package api

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// mountAdminStatic 托管 Vite 构建产物（basename=/admin）。
// 目录不存在时静默跳过，保持纯 API 模式。
func (s *Server) mountAdminStatic(mux *http.ServeMux) {
	dir := ""
	if s.cfg != nil {
		dir = strings.TrimSpace(s.cfg.AdminStaticDir)
	}
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return
	}
	index := filepath.Join(abs, "index.html")
	if _, err := os.Stat(index); err != nil {
		return
	}
	log.Printf("admin UI: serving %s at /admin/", abs)

	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/admin/")
		if rel == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.ServeFile(w, r, index)
			return
		}
		full := filepath.Clean(filepath.Join(abs, rel))
		if full != abs && !strings.HasPrefix(full, abs+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
			http.ServeFile(w, r, full)
			return
		}
		http.ServeFile(w, r, index)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
}
