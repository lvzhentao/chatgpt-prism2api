package prism

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// register hang 不得拖住 start：热路径（沙箱缓存命中）下 register 是
// fire-and-forget，start 请求必须远早于 register 独立超时（5s）到达上游。
func TestStartTurnNotBlockedByHangingRegister(t *testing.T) {
	chatHit := make(chan time.Time, 1)
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathConversationHistory:
			// 模拟上游 hang：一直占住连接，直到客户端超时放弃。
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		case PathChat:
			select {
			case chatHit <- time.Now():
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"r1","conversation_id":"c1","turn_state":{"prompt":"hi"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newHTTPClient(adapter.ClientConfig{
		BaseURL:       srv.URL,
		TokenProvider: func() (string, error) { return "oai-jwt", nil },
	})
	// 热路径：预置有效沙箱，startTurn 不走预热链。
	c.sandbox = "hot-tok"
	c.sandboxAt = time.Now()

	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := c.startTurn(ctx, nr, "proj-1")
	if err != nil {
		t.Fatalf("startTurn: %v", err)
	}
	if out.RequestID != "r1" {
		t.Fatalf("RequestID=%q want r1", out.RequestID)
	}
	select {
	case hit := <-chatHit:
		if d := hit.Sub(start); d >= registerTimeout {
			t.Fatalf("start blocked by hanging register: chat hit after %v (registerTimeout=%v)", d, registerTimeout)
		}
	case <-time.After(registerTimeout + 5*time.Second):
		t.Fatal("start request never reached upstream")
	}
}

// 轮询退避序列：前 3 次急 poll 100ms，之后 0.4/0.8/1.2/1.6/2.0，2s 封顶（T5.1）。
func TestPollIntervalSequence(t *testing.T) {
	want := map[int]time.Duration{
		1:  100 * time.Millisecond,
		2:  100 * time.Millisecond,
		3:  100 * time.Millisecond,
		4:  400 * time.Millisecond,
		5:  800 * time.Millisecond,
		6:  1200 * time.Millisecond,
		7:  1600 * time.Millisecond,
		8:  2 * time.Second,
		12: 2 * time.Second,
	}
	for n, w := range want {
		if got := pollInterval(n); got != w {
			t.Errorf("pollInterval(%d) = %v, want %v", n, got, w)
		}
	}
}
