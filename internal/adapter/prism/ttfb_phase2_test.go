package prism

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// prewarmMock 模拟预热链全部上游端点，记 backend/1/new 调用次数。
type prewarmMock struct {
	srv       *httptest.Server
	backendNW *atomic.Int64
}

func newPrewarmMock() *prewarmMock {
	m := &prewarmMock{backendNW: &atomic.Int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == PathSession:
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "sess-abc", Path: "/"})
			_, _ = w.Write([]byte(`{"user":{"email":"t@example.com","app_metadata":{"user_id":"u1"}}}`))
		// 注意：PathProjectList 带 "?section=..." query，r.URL.Path 已剥掉，只能按纯路径匹配。
		case p == "/api/file-management/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		case p == PathProjects:
			_, _ = w.Write([]byte(`{"uuid":"proj-1"}`))
		case p == PathBackendNew:
			m.backendNW.Add(1)
			// 沙箱基址指回本 mock，后续 proxy 调用都落到这里。
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"sbx-tok","url":%q}`, m.srv.URL+PathSandboxBase)))
		case strings.HasPrefix(p, "/api/projects/") && strings.HasSuffix(p, "/sandbox/resources-token"):
			_, _ = w.Write([]byte(`{"access_token":"rt-1"}`))
		case p == PathSandboxBase+PathProxyResources:
			_, _ = w.Write([]byte(`{}`))
		case p == PathYSweet:
			_, _ = w.Write([]byte(`{"url":"https://y.example/d","baseUrl":"https://y.example","docId":"proj-1","token":"y-tok","authorization":"y-auth"}`))
		case p == PathSandboxBase+PathProxyToken:
			_, _ = w.Write([]byte(`{}`))
		case p == PathSandboxBase+"/wait-for-sync":
			_, _ = w.Write([]byte(`{"status":"synced"}`))
		default:
			http.NotFound(w, r)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *prewarmMock) newClient() *httpClient {
	return newHTTPClient(adapter.ClientConfig{
		BaseURL:       m.srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
}

// T2.2：10 goroutine 同时调 prepareSandbox，只出 1 次 backend/1/new 网络。
func TestPrepareSandboxSingleflight(t *testing.T) {
	m := newPrewarmMock()
	defer m.srv.Close()
	c := m.newClient()

	const n = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			toks[i], errs[i] = c.prepareSandbox(ctx, "proj-1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: prepareSandbox: %v", i, errs[i])
		}
		if toks[i] != "sbx-tok" {
			t.Fatalf("goroutine %d: token=%q want sbx-tok", i, toks[i])
		}
	}
	if got := m.backendNW.Load(); got != 1 {
		t.Fatalf("backend/1/new calls=%d want 1 (singleflight 合并失败)", got)
	}
}

// T2.1：Prewarm 成功路径——session/project/sandbox 各阶段命中并缓存。
func TestPrewarmSuccess(t *testing.T) {
	m := newPrewarmMock()
	defer m.srv.Close()
	c := m.newClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	if !c.sessionValid() {
		t.Fatal("Prewarm 后 session 仍无效")
	}
	c.mu.Lock()
	proj := c.projectID
	c.mu.Unlock()
	if proj != "proj-1" {
		t.Fatalf("projectID=%q want proj-1", proj)
	}
	if tok := c.sandboxToken(); tok != "sbx-tok" {
		t.Fatalf("sandbox=%q want sbx-tok", tok)
	}
	if got := m.backendNW.Load(); got != 1 {
		t.Fatalf("backend/1/new calls=%d want 1", got)
	}

	// 第二次 Prewarm 全命中，不应再打 backend/1/new。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if err := c.Prewarm(ctx2); err != nil {
		t.Fatalf("second Prewarm: %v", err)
	}
	if got := m.backendNW.Load(); got != 1 {
		t.Fatalf("second Prewarm 后 backend/1/new calls=%d want 1（缓存未命中）", got)
	}
}

// T2.1 续：令牌年龄超过 sandboxRefreshAfter 时必须**主动重铸**，否则令牌到
// sandboxIdleTTL 就作废，两次 sweep 之间留下冷窗口（请求照样付 6~10s 冷链）。
func TestPrewarmRefreshesSandboxBeforeTTL(t *testing.T) {
	m := newPrewarmMock()
	defer m.srv.Close()
	c := m.newClient()

	old := sandboxRefreshAfter
	sandboxRefreshAfter = 20 * time.Millisecond
	defer func() { sandboxRefreshAfter = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	if got := m.backendNW.Load(); got != 1 {
		t.Fatalf("首次 Prewarm backend/1/new calls=%d want 1", got)
	}

	time.Sleep(30 * time.Millisecond)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if err := c.Prewarm(ctx2); err != nil {
		t.Fatalf("second Prewarm: %v", err)
	}
	if got := m.backendNW.Load(); got != 2 {
		t.Fatalf("令牌变旧后 backend/1/new calls=%d want 2（没有主动重铸）", got)
	}
	c.mu.Lock()
	age := time.Since(c.sandboxAt)
	c.mu.Unlock()
	if age > 10*time.Millisecond {
		t.Fatalf("重铸后沙箱年龄 %v 没有归零", age)
	}
}
