package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"prism-2api/internal/adapter"
	"prism-2api/internal/auth"
)

// 空 completion（上游假完成：整轮零正文/思考/工具调用）的换号重试与终态。
// 背景：线上 24h 约 900 例空完成以 200 成功回给客户端，NewAPI 面板记成 0 t/s
// 假成功，客户拿到空消息也无法重试。现在：换号重试（上限 emptyRetryMax），
// 仍空则流内错误帧 / 非流式 502，不再回静默空成功。

// sequencedStreamClient 按共享计数器驱动脚本：各账号各包一层、共用一个计数器，
// 模拟「第一次返空、第二次返正文」这类跨账号序列（与账号被选顺序无关）。
type sequencedStreamClient struct {
	adapter.Client
	calls  *int32
	script func(n int32, emit func(adapter.Event) bool)
}

func (c *sequencedStreamClient) Stream(_ context.Context, _ *adapter.NativeRequest, emit func(adapter.Event) bool) error {
	c.script(atomic.AddInt32(c.calls, 1), emit)
	return nil
}

func chatCompletionsPOST(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// wrapAccounts 建指定账号并给池里每个账号套上序列脚本客户端（共享计数器）。
func wrapAccounts(t *testing.T, s *Server, names []string, calls *int32, script func(n int32, emit func(adapter.Event) bool)) {
	t.Helper()
	for _, name := range names {
		if err := s.pool.SetToken(name, &auth.Token{AccessToken: "upstream-credential"}); err != nil {
			t.Fatalf("SetToken(%s): %v", name, err)
		}
	}
	for _, acc := range s.pool.Accounts() {
		acc.Client = &sequencedStreamClient{Client: acc.Client, calls: calls, script: script}
	}
}

func emitEndedOnly(emit func(adapter.Event) bool) { emit(adapter.Event{Ended: true}) }

// 空 completion 换号重试后应拿到正文，而不是把空 200 回给客户端。
func TestStreamChatEmptyCompletionRetriesNextAccount(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	var calls int32
	wrapAccounts(t, s, []string{"acct-a", "acct-b"}, &calls, func(n int32, emit func(adapter.Event) bool) {
		if n == 1 {
			emitEndedOnly(emit) // 第一轮（无论选中哪个号）都是空完成
			return
		}
		emit(adapter.Event{Text: "hello"})
		emitEndedOnly(emit)
	})

	w := chatCompletionsPOST(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "hello") {
		t.Fatalf("重试后应收到正文:\n%s", body)
	}
	if strings.Contains(body, "upstream_error") || strings.Contains(body, "empty completion") {
		t.Fatalf("重试成功的流里不该有错误帧:\n%s", body)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应恰好换号重试一次（共 2 次 Stream），实际 %d 次", got)
	}
}

// 全池持续返空时，流式终态必须是错误帧，不能是静默的空完成。
func TestStreamChatEmptyCompletionExhaustedIsError(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	var calls int32
	wrapAccounts(t, s, []string{"acct-a", "acct-b", "acct-c"}, &calls, func(_ int32, emit func(adapter.Event) bool) {
		emitEndedOnly(emit)
	})

	w := chatCompletionsPOST(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, "prism: upstream returned empty completion") {
		t.Fatalf("终态应带 empty completion 错误帧:\n%s", body)
	}
	if !strings.Contains(body, `"upstream_error"`) {
		t.Fatalf("终态错误帧类型应为 upstream_error:\n%s", body)
	}
	if got := atomic.LoadInt32(&calls); got != int32(emptyRetryMax+1) {
		t.Fatalf("应尝试 emptyRetryMax+1 轮后收尾，实际 %d 次", got)
	}
}

// 非流式：空 completion 换号重试后拿到正文。
func TestNonStreamChatEmptyCompletionRetriesNextAccount(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	var calls int32
	wrapAccounts(t, s, []string{"acct-a", "acct-b"}, &calls, func(n int32, emit func(adapter.Event) bool) {
		if n == 1 {
			emitEndedOnly(emit)
			return
		}
		emit(adapter.Event{Text: "hello"})
		emitEndedOnly(emit)
	})

	w := chatCompletionsPOST(t, s, `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, w.Body.String())
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "hello" {
		t.Fatalf("重试后应收到正文 hello: %+v", resp)
	}
	if resp.Usage.CompletionTokens <= 0 {
		t.Fatalf("重试成功 completion 应 >0，实际 %d", resp.Usage.CompletionTokens)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应恰好换号重试一次（共 2 次 Stream），实际 %d 次", got)
	}
}

// 非流式：全池持续返空 → 502 + 错误体，不是 200 空消息。
func TestNonStreamChatEmptyCompletionExhaustedIs502(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	var calls int32
	wrapAccounts(t, s, []string{"acct-a", "acct-b", "acct-c"}, &calls, func(_ int32, emit func(adapter.Event) bool) {
		emitEndedOnly(emit)
	})

	w := chatCompletionsPOST(t, s, `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("空完成终态应 502，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "empty completion") {
		t.Fatalf("错误体应说明空完成:\n%s", w.Body.String())
	}
}

// S1 拦截说明帧计入 completion：整轮只有受限说明送达时，completion 不应记 0
// （说明帧是真实交付内容；记 0 会在 NewAPI 面板落成「0 t/s 无输出」假账）。
func TestStreamChatBlockedToolNoteCountsCompletion(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	var calls int32
	wrapAccounts(t, s, []string{"acct-a"}, &calls, func(_ int32, emit func(adapter.Event) bool) {
		// 上游调了客户端没声明的工具 → S1 拦截，整轮只剩受限说明帧。
		emit(adapter.Event{ToolCall: &adapter.StreamedToolCall{
			ToolCallID: "call_1", Name: "read_file", RawArgs: `{"path":"x"}`}})
		emitEndedOnly(emit)
	})

	w := chatCompletionsPOST(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],`+
		`"tools":[{"type":"function","function":{"name":"exec","description":"run","parameters":{"type":"object"}}}]}`)
	body := w.Body.String()
	if !strings.Contains(body, "上游工具受限") {
		t.Fatalf("应发出受限说明帧:\n%s", body)
	}
	if strings.Contains(body, `"completion_tokens":0`) {
		t.Fatalf("说明帧已送达，completion 不应记 0:\n%s", body)
	}
}
