package prism

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// hardeningMock 复用各用例已验证的上游形状：预热链 + start + status。
// backend/new 按调用序号回不同 token（sbx-tok-1, sbx-tok-2, ...），
// 用来模拟两条 flight 先后完成、断言旧令牌不覆盖新令牌。
type hardeningMock struct {
	srv       *httptest.Server
	backendNW *atomic.Int64
	chatNW    *atomic.Int64
	statusNW  *atomic.Int64
	// statusJSON 非空时 PathStatus 返回它（默认回无 reasoning 的 completed）。
	statusJSON string
}

func newHardeningMock() *hardeningMock {
	m := &hardeningMock{backendNW: &atomic.Int64{}, chatNW: &atomic.Int64{}, statusNW: &atomic.Int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == PathSession:
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "sess-abc", Path: "/"})
			_, _ = w.Write([]byte(`{"user":{"email":"t@example.com","app_metadata":{"user_id":"u1"}}}`))
		case p == "/api/file-management/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		case p == PathProjects:
			_, _ = w.Write([]byte(`{"uuid":"proj-1"}`))
		case p == PathBackendNew:
			n := m.backendNW.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"sbx-tok-%d","url":%q}`, n, m.srv.URL+PathSandboxBase)))
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
		case p == PathConversationHistory:
			_, _ = w.Write([]byte(`{}`))
		case p == PathChat:
			m.chatNW.Add(1)
			_, _ = w.Write([]byte(`{"request_id":"r1","conversation_id":"c1","turn_state":{"prompt":"hi"}}`))
		case p == PathStatus:
			m.statusNW.Add(1)
			body := m.statusJSON
			if body == "" {
				body = `{"status":"completed","request_id":"r1","turn_state":{"prompt":"hi"},"response":{"payload":{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}}}`
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *hardeningMock) newClient() *httpClient {
	return newHTTPClient(adapter.ClientConfig{
		BaseURL:       m.srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
}

// 两条 flight 先后完成时，后完成的旧令牌不得覆盖先发布的新令牌。
// 用 publishSandboxAt 注入假时钟做确定性断言（不依赖真实链条耗时）。
func TestPublishSandboxStaleFlightDoesNotOverwrite(t *testing.T) {
	m := newHardeningMock()
	defer m.srv.Close()
	c := m.newClient()

	base := time.Now()
	// 两条 flight 入 flight 前都读到同一个 entryAt（都没有新令牌）。
	entryAt := base
	// 快的那条先发布：sandboxAt=base+2s。
	if got := c.publishSandboxAt("sbx-new", m.srv.URL, entryAt, base.Add(2*time.Second)); got != "sbx-new" {
		t.Fatalf("先发布应返回新令牌，got %q", got)
	}
	// 慢的那条（链条启动更早、完成更晚）后发布：必须丢弃，返回当前生效的新令牌。
	if got := c.publishSandboxAt("sbx-old", m.srv.URL, entryAt, base.Add(3*time.Second)); got != "sbx-new" {
		t.Fatalf("后完成的旧令牌应被丢弃并返回现行令牌，got %q", got)
	}
	if tok := c.sandboxToken(); tok != "sbx-new" {
		t.Fatalf("缓存令牌=%q want sbx-new（旧令牌覆盖了新令牌）", tok)
	}
}

// 同一代际（entryAt 等于当前 sandboxAt）正常写入：请求路径与预热路径都要过这个发布函数。
func TestPublishSandboxSameGenerationWrites(t *testing.T) {
	m := newHardeningMock()
	defer m.srv.Close()
	c := m.newClient()

	base := time.Now()
	c.sandbox = "sbx-cur"
	c.sandboxAt = base
	// entryAt == sandboxAt：没有更新的发布，允许写入。
	if got := c.publishSandboxAt("sbx-next", m.srv.URL, base, base.Add(time.Second)); got != "sbx-next" {
		t.Fatalf("同代际应写入并返回新令牌，got %q", got)
	}
	if tok := c.sandboxToken(); tok != "sbx-next" {
		t.Fatalf("缓存令牌=%q want sbx-next", tok)
	}
}

// follower 的预算 ctx 一结束就立即返回，不再等 leader 跑完（Do 不感知 context 的穿透修复）。
// backend/new 由 release 门控：follower 调用时 leader 必定还在飞，避免“leader 已跑完、
// follower 直接拿到成功结果”的时序竞态。
func TestSandboxFlightFollowerRespectsBudgetCancel(t *testing.T) {
	release := make(chan struct{})
	backendStarted := make(chan struct{}, 1)
	m := newHardeningMock()
	defer m.srv.Close()
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == PathBackendNew {
			select {
			case backendStarted <- struct{}{}:
			default:
			}
			select {
			case <-release: // leader 放行后才回真正的 token
			case <-r.Context().Done():
				return
			}
		}
		m.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer gated.Close()
	c := newHTTPClient(adapter.ClientConfig{
		BaseURL:       gated.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})

	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.prepareSandbox(context.Background(), "proj-1")
		leaderDone <- err
	}()
	select {
	case <-backendStarted: // leader 已占住 flight 且卡在 backend/new
	case <-time.After(5 * time.Second):
		t.Fatal("leader 没有发出 backend/new")
	}

	fctx, cancel := context.WithCancel(context.Background())
	cancel() // 预算已过期：follower 应立即返回，不陪 leader 跑完。
	at := time.Now()
	_, err := c.prepareSandbox(fctx, "proj-1")
	if elapsed := time.Since(at); elapsed > 3*time.Second {
		t.Fatalf("follower 应随 ctx 立即返回，实际等了 %.1fs", elapsed.Seconds())
	}
	if err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader 应正常跑完链条，got %v", err)
	}
}

// session flight 同样 ctx 可抢占：leader 在飞时 follower 取消立即返回。
func TestSessionFlightFollowerRespectsCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSession {
			time.Sleep(300 * time.Millisecond)
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "sess-abc", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user":{"email":"t@example.com","app_metadata":{"user_id":"u1"}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newHTTPClient(adapter.ClientConfig{
		BaseURL:       srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- c.ensureSession(context.Background()) }()
	time.Sleep(50 * time.Millisecond)

	fctx, cancel := context.WithCancel(context.Background())
	cancel()
	at := time.Now()
	err := c.ensureSession(fctx)
	if elapsed := time.Since(at); elapsed > 3*time.Second {
		t.Fatalf("follower 应随 ctx 立即返回，实际等了 %.1fs", elapsed.Seconds())
	}
	if err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader 应正常铸造 session，got %v", err)
	}
}

// start_calls/poll_calls 计数：一次完整 turn 里 start 打 1 次、status 打 1 次，
// upstream_calls=start_calls+poll_calls。
func TestUpstreamCallCountsSingleTurn(t *testing.T) {
	m := newHardeningMock()
	defer m.srv.Close()
	c := m.newClient()

	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	start, err := c.startTurn(context.Background(), nr, "proj-1")
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if start.StartCalls != 1 {
		t.Fatalf("StartCalls=%d want 1", start.StartCalls)
	}
	res, err := c.pollTurn(context.Background(), start, func(adapter.Event) bool { return true })
	if err != nil {
		t.Fatalf("pollTurn: %v", err)
	}
	if res.Polls != 1 {
		t.Fatalf("Polls=%d want 1", res.Polls)
	}
	if got := start.StartCalls + res.Polls; got != 2 {
		t.Fatalf("upstream_calls=%d want 2", got)
	}
	if m.chatNW.Load() != 1 || m.statusNW.Load() != 1 {
		t.Fatalf("上游调用 chat=%d status=%d，各 want 1", m.chatNW.Load(), m.statusNW.Load())
	}
}
