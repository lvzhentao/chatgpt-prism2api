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

// T3.3 mock：冷链可按需失败 backend/1/new 前 failN 次（400：requestRetry
// 不重试 4xx，失败直达 prepareSandboxUnshared 调用方），之后成功；其余
// 预热链 + register + start 全部走通。记 backend/1/new 与 start 调用次数。
type prepareRetryMock struct {
	srv        *httptest.Server
	backendNW  *atomic.Int64
	chatNW     *atomic.Int64
	failBudget *atomic.Int64
}

func newPrepareRetryMock(failN int64) *prepareRetryMock {
	m := &prepareRetryMock{
		backendNW:  &atomic.Int64{},
		chatNW:     &atomic.Int64{},
		failBudget: &atomic.Int64{},
	}
	m.failBudget.Store(failN)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == PathBackendNew:
			m.backendNW.Add(1)
			if m.failBudget.Add(-1) >= 0 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"cold chain hiccup"}`))
				return
			}
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
		case p == PathConversationHistory:
			_, _ = w.Write([]byte(`{}`))
		case p == PathChat:
			m.chatNW.Add(1)
			_, _ = w.Write([]byte(`{"request_id":"r1","conversation_id":"c1","turn_state":{"prompt":"hi"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *prepareRetryMock) newClient() *httpClient {
	return newHTTPClient(adapter.ClientConfig{
		BaseURL:       m.srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
}

// T3.3：prepare 前 2 次失败第 3 次成功 → startTurn 就地退避后不换号成功
// （start 只打到上游 1 次，backend/1/new 共 3 次）。
func TestStartTurnPrepareRetryThenSuccess(t *testing.T) {
	m := newPrepareRetryMock(2)
	defer m.srv.Close()
	c := m.newClient()

	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := c.startTurn(ctx, nr, "proj-1")
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if out.RequestID != "r1" {
		t.Fatalf("RequestID=%q want r1", out.RequestID)
	}
	if got := m.backendNW.Load(); got != 3 {
		t.Fatalf("backend/1/new calls=%d want 3（初次+就地重试2次）", got)
	}
	if got := m.chatNW.Load(); got != 1 {
		t.Fatalf("start calls=%d want 1（ prepare 退避期间不得换号重发 start）", got)
	}
}

// T3.3：prepare 持续失败 → 退避耗尽后返回错误，重试次数符合预期
// （backend/1/new 共 3 次 = 初次 + ≤2 次就地重试），且 start 从未发出。
func TestStartTurnPrepareRetryExhausted(t *testing.T) {
	m := newPrepareRetryMock(1 << 30)
	defer m.srv.Close()
	c := m.newClient()

	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := c.startTurn(ctx, nr, "proj-1"); err == nil {
		t.Fatal("startTurn 成功了，want prepare 持续失败后的透出错误")
	}
	if got := m.backendNW.Load(); got != 3 {
		t.Fatalf("backend/1/new calls=%d want 3（初次+就地重试2次，不多不少）", got)
	}
	if got := m.chatNW.Load(); got != 0 {
		t.Fatalf("start calls=%d want 0（沙箱没出来不得发 start）", got)
	}
}
