package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-2api/internal/scheduler"
)

func TestInternalChatRequestPreservesSessionAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orig := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(`{}`))
	orig = orig.WithContext(ctx)
	orig.Header.Set("X-Session-ID", "sess-abc")
	orig.Header.Set("Session-Id", "codex-1")
	orig.Header.Set("Content-Type", "text/plain")

	got := internalChatRequest(orig, []byte(`{"model":"x"}`))
	if got.Method != http.MethodPost {
		t.Fatalf("method = %s", got.Method)
	}
	if got.URL.Path != "/v1/chat/completions" {
		t.Fatalf("path = %s", got.URL.Path)
	}
	if got.Header.Get("X-Session-ID") != "sess-abc" {
		t.Fatalf("X-Session-ID = %q", got.Header.Get("X-Session-ID"))
	}
	if got.Header.Get("Session-Id") != "codex-1" {
		t.Fatalf("Session-Id = %q", got.Header.Get("Session-Id"))
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", got.Header.Get("Content-Type"))
	}

	cancel()
	if err := got.Context().Err(); err != context.Canceled {
		t.Fatalf("context err = %v, want canceled", err)
	}

	bare := internalChatRequest(nil, []byte(`{"model":"x"}`))
	if bare == nil || bare.URL.Path != "/v1/chat/completions" {
		t.Fatalf("nil r request = %+v", bare)
	}
	if bare.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("nil r Content-Type = %q", bare.Header.Get("Content-Type"))
	}
}

func TestResponsesToChatRequestPreservesSessionFields(t *testing.T) {
	req := &ResponsesRequest{
		Model:          "gpt-5",
		Input:          json.RawMessage(`"hello"`),
		PromptCacheKey: "cache-1",
		Metadata:       map[string]any{"user_id": "user-plain"},
		Conversation:   json.RawMessage(`{"id":"c1"}`),
		ConversationID: "conv-navos",
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	body, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	primary, fallback := scheduler.ExtractSessionIDs(nil, body)
	if primary != "pck:cache-1" || fallback != "conv:c1" {
		t.Fatalf("pck/conv = %q / %q body=%s", primary, fallback, body)
	}
	if chatReq.ConversationID != "conv-navos" {
		t.Fatalf("conversation_id = %q", chatReq.ConversationID)
	}
}

// responses 的 input_text 数组必须摊平成 chat 可读的 text 块（V3 真机问题：
// 原样透传后 contentText 读不到文本，上游只看到空 user 消息）。
func TestResponsesInputTextFlattened(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"你是谁"}]}]`),
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(chatReq.Messages))
	}
	if got := contentText(chatReq.Messages[0].Content); got != "你是谁" {
		t.Fatalf("content 应摊平成 text 块，got %q (raw %s)", got, chatReq.Messages[0].Content)
	}
}

// function_call_output 的 output 允许字符串或 content-part 数组（SDK 回灌常见
// 数组形态）。string 类型字段会在此直接 400，必须是 any + 摊平（线上真机问题）。
func TestResponsesFunctionCallOutputArrayShape(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"北京 25℃ 晴"}]}
		]`),
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("数组 output 不应再 400: %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(chatReq.Messages))
	}
	tool := chatReq.Messages[1]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" {
		t.Fatalf("回灌消息形状错误: %+v", tool)
	}
	if got := contentText(tool.Content); got != "北京 25℃ 晴" {
		t.Fatalf("数组 output 应摊平成文本，got %q", got)
	}
}

func TestResponsesFunctionCallOutputStringShape(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`[
			{"type":"function_call_output","call_id":"call_2","output":"纯文本结果"}
		]`),
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := contentText(chatReq.Messages[0].Content); got != "纯文本结果" {
		t.Fatalf("字符串 output 应原样透传，got %q", got)
	}
}

// input 为单个条目对象（不包数组）时不得 400（SDK 常见写法；
// "cannot unmarshal object into []responsesInputItem" 线上真机问题）。
func TestResponsesSingleObjectInput(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`{"type":"message","role":"user","content":"hi"}`),
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("单对象 input 不应 400: %v", err)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(chatReq.Messages))
	}
	if got := contentText(chatReq.Messages[0].Content); got != "hi" {
		t.Fatalf("content=%q", got)
	}
}

// function_call 的 arguments 允许 JSON 字符串或对象（Codex 等直接传对象；
// "cannot unmarshal object into ...arguments of type string" 线上真机问题）。
func TestResponsesFunctionCallArgumentsObjectShape(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_3","name":"get_weather","arguments":{"city":"北京"}}
		]`),
	}
	chatReq, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatalf("对象 arguments 不应 400: %v", err)
	}
	tc := chatReq.Messages[0].ToolCalls
	if len(tc) != 1 {
		t.Fatalf("want 1 tool_call, got %+v", chatReq.Messages)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments 应归一化成 JSON 字符串，got %q", tc[0].Function.Arguments)
	}
	if args["city"] != "北京" {
		t.Fatalf("arguments 内容丢失: %v", args)
	}
}
