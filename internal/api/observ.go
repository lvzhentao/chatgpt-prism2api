package api

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/failclass"
	"prism-2api/internal/pool"
)

func requestIDFrom(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" {
		return id
	}
	if id := strings.TrimSpace(r.Header.Get("X-Request-Id")); id != "" {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type bodyCapture struct {
	io.ReadCloser
	head  []byte
	tail  []byte
	total int
}

func (c *bodyCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		chunk := p[:n]
		if len(c.head) < bodyHeadBytes {
			need := bodyHeadBytes - len(c.head)
			if need > len(chunk) {
				need = len(chunk)
			}
			c.head = append(c.head, chunk[:need]...)
		}
		if len(chunk) >= bodyTailBytes {
			c.tail = append([]byte(nil), chunk[len(chunk)-bodyTailBytes:]...)
		} else {
			c.tail = append(c.tail, chunk...)
			if len(c.tail) > bodyTailBytes {
				c.tail = c.tail[len(c.tail)-bodyTailBytes:]
			}
		}
		c.total += n
	}
	return n, err
}

func (c *bodyCapture) loggedBody() string {
	if c == nil || c.total == 0 {
		return ""
	}
	if c.total <= len(c.tail) {
		return admin.RedactRequestBody(c.tail)
	}
	skipped := c.total - len(c.head) - len(c.tail)
	if skipped < 0 {
		skipped = 0
	}
	return admin.RedactText(string(c.head)) + "\n...[truncated " + fmt.Sprintf("%d", skipped) + " chars]...\n" + admin.RedactText(string(c.tail))
}

const (
	bodyHeadBytes = 8 * 1024
	bodyTailBytes = 24 * 1024
)

func wrapBodyCapture(r *http.Request) *bodyCapture {
	if r == nil || r.Body == nil {
		return nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	c := &bodyCapture{ReadCloser: r.Body}
	r.Body = c
	return c
}

func fillLog(entry *admin.LogEntry, acc *pool.Account, iter *accountIter, class failclass.Class) {
	if entry == nil {
		return
	}
	if acc != nil {
		entry.Account = acc.Name
	}
	if class != "" {
		entry.FailClass = string(class)
	}
	if iter != nil {
		entry.RetryCount = iter.retryCount()
	}
}

func logRequestID(entry *admin.LogEntry) string {
	if entry == nil {
		return ""
	}
	return entry.RequestID
}

func (it *accountIter) retryCount() int {
	if it == nil {
		return 0
	}
	n := len(it.tried)
	if n > 0 {
		return n - 1 + it.outer
	}
	return it.outer
}

func (s *Server) recordClientUsage(r *http.Request, prompt, completion int) {
	if s == nil || s.keys == nil || r == nil {
		return
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if prompt == 0 && completion == 0 {
		return
	}
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return
	}
	s.keys.RecordUsage(id.KeyID, uint64(prompt), uint64(completion), 0, 0, 0)
}

func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) requireLocalhost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackAddr(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

func (s *Server) mountPprof(mux *http.ServeMux) {
	wrap := s.requireLocalhost
	mux.HandleFunc("/debug/pprof/", wrap(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", wrap(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", wrap(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", wrap(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", wrap(pprof.Trace))
	mux.HandleFunc("/debug/pprof/allocs", wrap(pprof.Handler("allocs").ServeHTTP))
	mux.HandleFunc("/debug/pprof/block", wrap(pprof.Handler("block").ServeHTTP))
	mux.HandleFunc("/debug/pprof/goroutine", wrap(pprof.Handler("goroutine").ServeHTTP))
	mux.HandleFunc("/debug/pprof/heap", wrap(pprof.Handler("heap").ServeHTTP))
	mux.HandleFunc("/debug/pprof/mutex", wrap(pprof.Handler("mutex").ServeHTTP))
	mux.HandleFunc("/debug/pprof/threadcreate", wrap(pprof.Handler("threadcreate").ServeHTTP))
}
