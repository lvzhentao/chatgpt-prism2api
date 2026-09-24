package prism

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/failclass"
)

// degradedMock 模拟劣化期的上游：`/api/backend/1/new`（预热链的第一跳）挂死
// ——真机劣化时实测 24~210s 无响应甚至挂死；sessionStatus 非 0 时 `/auth/session`
// 直接回它（劣化期实测这个端点被 Cloudflare 回 504）。其余端点不在用例路径上。
type degradedMock struct {
	srv           *httptest.Server
	sessionStatus int
}

func newDegradedMock() *degradedMock {
	m := &degradedMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSession && m.sessionStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(m.sessionStatus)
			_, _ = fmt.Fprintf(w, `{"title":"Error %d: Gateway time-out","status":%d}`,
				m.sessionStatus, m.sessionStatus)
			return
		}
		if r.URL.Path == PathBackendNew {
			// 先把请求体读完：服务端要先读到 body EOF 才会启动后台读，从而感知客户端断开，
			// 否则挂了就一直占着连接（用例结束时的 srv.Close() 会被它拖住）。
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done(): // 一直挂到客户端放弃
			case <-time.After(5 * time.Second): // 兜底：用例失败也不把整个包挂住
			}
			return
		}
		http.NotFound(w, r)
	}))
	return m
}

func (m *degradedMock) newClient() *httpClient {
	return newHTTPClient(adapter.ClientConfig{
		BaseURL:       m.srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
}

// 上游 backend 劣化时（实测 /api/backend/1/new 24~210s 甚至挂死）：预热链必须快速失败，
// 而不是把逐跳 3×75s 的重试阶梯耗完 —— 客户端握着连接等 3 分钟比拿到 503 糟得多。
func TestWarmupBudgetFailsFastWhenBackendDegraded(t *testing.T) {
	old := warmupBudget
	warmupBudget = 200 * time.Millisecond
	defer func() { warmupBudget = old }()

	m := newDegradedMock()
	defer m.srv.Close()
	c := m.newClient()

	at := time.Now()
	_, err := c.prepareSandbox(context.Background(), "proj-1")
	elapsed := time.Since(at)

	if err == nil {
		t.Fatal("backend 挂死时应当失败")
	}
	if !isDegraded(err) {
		t.Fatalf("应报成劣化（degradedError），得到 %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("应当快速失败，实际耗时 %.1fs", elapsed.Seconds())
	}
	res := failclass.Classify(err)
	if res.Cool || res.Switch || res.HTTPStatus != http.StatusServiceUnavailable {
		t.Errorf("劣化分类应同维护（Unavailable / 不冷却 / 不换号），得到 %+v", res)
	}
}

// 调用方自己取消时不能报成劣化（否则客户端断开会被记成上游故障，还可能触发限流判断）。
func TestWarmupBudgetDoesNotBlameUpstreamOnClientCancel(t *testing.T) {
	old := warmupBudget
	warmupBudget = 5 * time.Second
	defer func() { warmupBudget = old }()

	m := newDegradedMock()
	defer m.srv.Close()
	c := m.newClient()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := c.prepareSandbox(ctx, "proj-1")
	if err == nil {
		t.Fatal("取消后应当失败")
	}
	if isDegraded(err) {
		t.Fatalf("客户端取消不该报成上游劣化: %v", err)
	}
}

// session 铸造撞 edge→origin 的 5xx（劣化期实测 Cloudflare 504）：按劣化快速失败，
// 不要换号重试 —— 别的账号打的是同一个 origin，实测白等 ~30s/次。
func TestSessionMint5xxIsDegraded(t *testing.T) {
	m := newDegradedMock()
	defer m.srv.Close()
	m.sessionStatus = 504
	c := m.newClient()

	err := c.ensureSession(context.Background())
	if err == nil {
		t.Fatal("session 504 时应当失败")
	}
	if !isDegraded(err) {
		t.Fatalf("应报成劣化，得到 %v", err)
	}
	if res := failclass.Classify(err); res.Switch || res.Cool {
		t.Errorf("劣化不该换号/冷却，得到 %+v", res)
	}
}
