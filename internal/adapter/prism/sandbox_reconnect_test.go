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

// reconnectMock 模拟沙箱刚建好、工作区还在 sync 的窗口：前 N 次 start 回
// sandbox_reconnecting（真机形状，无 turn_state），之后成功；codex/healthz 报 ok。
// 记 backend/1/new 与 start 调用次数，用来验证「重试同一个沙箱、不重新预热」。
type reconnectMock struct {
	srv          *httptest.Server
	backendNW    *atomic.Int64
	chatNW       *atomic.Int64
	reconnecting *atomic.Int64
}

func newReconnectMock(reconnectingTimes int64) *reconnectMock {
	m := &reconnectMock{backendNW: &atomic.Int64{}, chatNW: &atomic.Int64{}, reconnecting: &atomic.Int64{}}
	m.reconnecting.Store(reconnectingTimes)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
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
		case p == PathSandboxBase+"/codex/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case p == PathConversationHistory:
			_, _ = w.Write([]byte(`{}`))
		case p == PathChat:
			m.chatNW.Add(1)
			if m.reconnecting.Add(-1) >= 0 {
				_, _ = w.Write([]byte(`{"status":"completed","request_id":"r0","response":{"status":"error","payload":{"reason":"sandbox_reconnecting","message":"Reconnecting to sandbox. Your request will resume automatically once the sandbox is ready."}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"request_id":"r1","conversation_id":"c1","turn_state":{"prompt":"hi"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *reconnectMock) newClient() *httpClient {
	return newHTTPClient(adapter.ClientConfig{
		BaseURL:       m.srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
}

// speedUpReconnect 把 reconnecting 路径的节奏参数压到毫秒级（不是真实等待时长；
// 生产值见 client.go 的沙箱节奏参数）。
func speedUpReconnect(t *testing.T) {
	t.Helper()
	origWait, origBudget, origPoll := sandboxReconnectWait, codexReadyBudget, codexReadyPoll
	sandboxReconnectWait, codexReadyBudget, codexReadyPoll = time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		sandboxReconnectWait, codexReadyBudget, codexReadyPoll = origWait, origBudget, origPoll
	})
}

// 沙箱刚建好时第一次 start 常回 sandbox_reconnecting。此时**不能换沙箱** ——
// 换掉等于白付一次 6~18s 预热；正确做法是等 codex 就绪后重试同一个沙箱
// （参考实现 prism-proxy 的做法）。
func TestSandboxReconnectingRetriesSameSandbox(t *testing.T) {
	speedUpReconnect(t)
	m := newReconnectMock(2) // 前两次都回 reconnecting，第三次才成功
	defer m.srv.Close()
	c := m.newClient()

	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	out, err := c.startTurn(context.Background(), nr, "proj-1")
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if out.RequestID != "r1" {
		t.Fatalf("RequestID=%q want r1", out.RequestID)
	}
	if n := m.backendNW.Load(); n != 1 {
		t.Errorf("reconnecting 期间不该重新预热沙箱：backend/1/new 调用 %d 次，want 1", n)
	}
	if n := m.chatNW.Load(); n != 3 {
		t.Errorf("start 调用 %d 次，want 3（两次 reconnecting + 一次成功）", n)
	}
}

// 反复 reconnecting（超过上限）时要停下，不能无限重试。
func TestSandboxReconnectingGivesUpAfterLimit(t *testing.T) {
	speedUpReconnect(t)
	m := newReconnectMock(99)
	defer m.srv.Close()
	c := m.newClient()

	const attemptsCap int64 = 3 // startTurn 的 attempts
	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	if _, err := c.startTurn(context.Background(), nr, "proj-1"); err == nil {
		t.Fatal("一直 reconnecting 时应当失败")
	}
	// 上限 = 每次 attempt 最多 maxSandboxReconnects 次 reconnect，共 attempts 个 attempt。
	// 关键是**有界且会终止**：不能因为一直 reconnecting 就无限重试（fake 允许 99 次）。
	if n := m.chatNW.Load(); n > attemptsCap*int64(maxSandboxReconnects+1) {
		t.Errorf("重试次数应当受上限约束，start 调用了 %d 次", n)
	}
}
