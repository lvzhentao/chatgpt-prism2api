// Package debugtrace 把一次请求的入站 HTTP、上游 Agent 帧、转发出的 SSE
// 按时间序写成 JSONL。默认关闭。
//
// 启用：WEB2API_DEBUG=1（目录 local/debug）或 WEB2API_DEBUG_DIR=<dir>
// 或 --debug-dir。
package debugtrace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type ctxKey struct{}

var (
	mu   sync.Mutex
	dir  string
	seen bool
)

// Configure 指定输出目录并启用。空字符串表示关闭。
func Configure(d string) {
	mu.Lock()
	defer mu.Unlock()
	dir = strings.TrimSpace(d)
	seen = true
}

// Enabled 是否正在写 debug JSONL。
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	if !seen {
		seen = true
		if d := strings.TrimSpace(os.Getenv("WEB2API_DEBUG_DIR")); d != "" {
			dir = d
		} else {
			switch strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_DEBUG"))) {
			case "1", "true", "yes", "on":
				dir = "local/debug"
			}
		}
	}
	return dir != ""
}

// Dir 返回当前输出目录（未启用则为空）。
func Dir() string {
	mu.Lock()
	defer mu.Unlock()
	return dir
}

// Session 对应一次 HTTP 请求的 JSONL 文件。
type Session struct {
	ID   string
	Path string
	t0   time.Time
	mu   sync.Mutex
	f    *os.File
}

// Start 创建会话文件并写入 http_request。headers 应已脱敏。
func Start(id, method, path, query string, headers map[string]string, body []byte) *Session {
	if !Enabled() {
		return nil
	}
	mu.Lock()
	outDir := dir
	mu.Unlock()
	if outDir == "" {
		return nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil
	}
	if id == "" {
		id = time.Now().Format("150405.000")
	}
	safePath := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, strings.TrimPrefix(path, "/"))
	if safePath == "" {
		safePath = "root"
	}
	name := time.Now().Format("20060102-150405.000") + "_" + method + "_" + safePath + "_" + sanitizeID(id) + ".jsonl"
	fp := filepath.Join(outDir, name)
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	s := &Session{ID: id, Path: fp, t0: time.Now(), f: f}
	s.Emit("http_request", map[string]any{
		"method":  method,
		"path":    path,
		"query":   query,
		"headers": headers,
		"body":    decodeBody(body),
	})
	return s
}

func sanitizeID(id string) string {
	id = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, id)
	if len(id) > 36 {
		return id[:36]
	}
	return id
}

// With 把 Session 放进 context。
func With(ctx context.Context, s *Session) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext 取出 Session（可能为 nil）。
func FromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}

// Emit 写一条带相对时间戳的事件。s 为 nil 时是空操作。
func (s *Session) Emit(kind string, payload any) {
	if s == nil || s.f == nil {
		return
	}
	rec := map[string]any{
		"t_ms": time.Since(s.t0).Milliseconds(),
		"at":   time.Now().Format(time.RFC3339Nano),
		"kind": kind,
	}
	if payload != nil {
		rec["data"] = payload
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"kind": kind, "error": err.Error()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(append(b, '\n'))
}

// Emit 从 context 写事件。
func Emit(ctx context.Context, kind string, payload any) {
	FromContext(ctx).Emit(kind, payload)
}

// Finish 写 http_done 并关闭文件。
func (s *Session) Finish(payload any) {
	if s == nil {
		return
	}
	s.Emit("http_done", payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
}

// RedactHeaders 复制请求头并把鉴权类字段打码。
func RedactHeaders(h map[string][]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		v := strings.Join(vs, ",")
		switch lk {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "x-master-key":
			out[lk] = "***"
		default:
			out[lk] = v
		}
	}
	return out
}

func decodeBody(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		var v any
		if json.Unmarshal(b, &v) == nil {
			return v
		}
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return map[string]any{"bytes": len(b), "note": "non-utf8"}
}
