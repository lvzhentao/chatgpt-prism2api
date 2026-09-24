package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetersCountAndWindow(t *testing.T) {
	m := newMeters()
	m.Incoming()
	m.Incoming()
	m.Done("gpt-5.6-terra", 200, 100, 50, 2)
	m.Done("gpt-5.6-terra", 500, 0, 0, 1)

	snap := m.Snapshot()
	g := snap["global"].(meterStats)
	if g.RPM != 2 {
		t.Fatalf("rpm=%d want 2", g.RPM)
	}
	if g.ServedRPM != 1 || g.RejectedRPM != 1 {
		t.Fatalf("served=%d rejected=%d want 1/1", g.ServedRPM, g.RejectedRPM)
	}
	if g.TPM != 150 {
		t.Fatalf("tpm=%d want 150", g.TPM)
	}
	if g.RetryPerReq != 1.5 {
		t.Fatalf("retry_per_req=%v want 1.5", g.RetryPerReq)
	}
	byModel := snap["by_model"].(map[string]meterStats)
	if byModel["gpt-5.6-terra"].RPM != 2 {
		t.Fatalf("model rpm=%d want 2", byModel["gpt-5.6-terra"].RPM)
	}
	if snap["inflight"].(int64) != 0 {
		t.Fatalf("inflight=%d want 0", snap["inflight"])
	}
}

func TestMetersBucketRollover(t *testing.T) {
	m := newMeters()
	m.Incoming()
	// 快进 headSec：手工把全局序列滚到 120 秒前打过数据的位置，验证窗口淘汰。
	m.mu.Lock()
	m.global.advance(time.Now().Unix() - 1)
	m.global.buckets[(time.Now().Unix()-1)%meterBuckets].served = 5
	m.mu.Unlock()
	m.Done("m", 200, 0, 0, 0)

	snap := m.Snapshot()
	g := snap["global"].(meterStats)
	if g.ServedRPM != 6 {
		t.Fatalf("served=%d want 6 (5+1 同窗口)", g.ServedRPM)
	}
}

func TestMetersInflightGauge(t *testing.T) {
	m := newMeters()
	m.Incoming()
	m.Incoming()
	m.Incoming()
	if m.inflight != 1 && m.inflight != 3 {
		// 并发下介于 1~3 都合法，这里串行调用应为 3
		t.Fatalf("inflight=%d want 3", m.inflight)
	}
	m.Done("x", 200, 0, 0, 0)
	m.Done("x", 200, 0, 0, 0)
	m.Done("x", 200, 0, 0, 0)
	if m.inflight != 0 {
		t.Fatalf("inflight=%d want 0", m.inflight)
	}
}

func TestGateDirectWhenNoCapacity(t *testing.T) {
	// 空池/未配容量：闸门直通（nil, true），绝不阻塞。
	s := &Server{}
	ch, ok := s.gateAcquire(t.Context())
	if !ok || ch != nil {
		t.Fatalf("want (nil,true), got (%v,%v)", ch, ok)
	}
}

func TestGateAcquireAndRelease(t *testing.T) {
	s := &Server{gate: make(chan struct{}, 1), gateCheckedAt: time.Now().Add(time.Hour)} // 跳过重算
	ch, ok := s.gateAcquire(t.Context())
	if !ok || ch == nil || cap(ch) != 1 {
		t.Fatalf("want acquire on cap-1 gate, got (%v,%v)", ch, ok)
	}
	if len(ch) != 1 {
		t.Fatalf("slot not held, len=%d", len(ch))
	}
	<-ch // 释放（同中间件 defer 语义）
	if len(ch) != 0 {
		t.Fatalf("slot not released, len=%d", len(ch))
	}
}

func TestGateRejectsWhenFullAndCtxExpires(t *testing.T) {
	// 槽占满 + 客户端 ctx 先到期 → 返回 false（中间件据此回 429+Retry-After）。
	s := &Server{gate: make(chan struct{}, 1), gateCheckedAt: time.Now().Add(time.Hour)}
	s.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	ch, ok := s.gateAcquire(ctx)
	if ok || ch != nil {
		t.Fatalf("want rejection, got (%v,%v)", ch, ok)
	}
}

func TestGateRejectionReturns429(t *testing.T) {
	// 端到端：占满闸门后请求收到 429 + Retry-After（日志/计量路径自然覆盖）。
	s := setupTestServer(t)
	s.gateMu.Lock()
	s.gate = make(chan struct{}, 1)
	s.gate <- struct{}{} // 占满
	s.gateCheckedAt = time.Now().Add(time.Hour)
	s.gateMu.Unlock()
	handler := s.Handler()
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
}
