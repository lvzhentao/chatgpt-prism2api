package prism

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

// withoutInjectedPrompt 关掉本网关注入的系统指令（prompt.go），
// 让这些用例只验证「历史折叠 / 块顺序」自身的语义（注入路径另有 prompt_test.go）。
func withoutInjectedPrompt(t *testing.T) {
	t.Helper()
	t.Setenv("PRISM_SYSTEM_PROMPT", "off")
}

// 上游只把最后一条 user 消息当本轮请求；历史必须折叠进 Context，
// 否则模型看不到上下文（真机验证：漏折叠时答非所问）。
func TestBuildInputFoldsHistoryIntoContext(t *testing.T) {
	withoutInjectedPrompt(t)
	nr := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{
			{Role: "system", Content: "回答尽量简短。"},
			{Role: "user", Content: "中国首都？"},
			{Role: "assistant", Content: "北京"},
			{Role: "user", Content: "它有多少人口？"},
		},
	}
	items := buildInput(nr)
	if len(items) != 2 {
		t.Fatalf("want 2 items (合并 system + user)，got %d: %s", len(items), dump(items))
	}
	ctx := itemText(t, items[0])
	if itemRole(t, items[0]) != "system" {
		t.Fatalf("item 0 should be the merged system block, got role %q", itemRole(t, items[0]))
	}
	for _, want := range []string{"[客户端指令]\n回答尽量简短。", "[对话历史]", "User: 中国首都？", "Assistant: 北京"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("合并 system 块缺少 %q: %s", want, dump(items))
		}
	}
	if strings.Contains(ctx, "它有多少人口？") {
		t.Fatalf("last user message must not be duplicated into the history block: %s", ctx)
	}
	if last := items[len(items)-1]; itemRole(t, last) != "user" || itemText(t, last) != "它有多少人口？" {
		t.Fatalf("last item must be the current user request, got %s", dump(items))
	}
}

func TestBuildInputSingleTurnHasNoContextBlock(t *testing.T) {
	withoutInjectedPrompt(t)
	items := buildInput(&adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}})
	if len(items) != 1 {
		t.Fatalf("single-turn request must produce exactly one item, got %s", dump(items))
	}
}

// 工具结果对上游没有原生表示，必须带上，且要单独成块（真机验证：混在
// [对话历史] 里时，声明了 tools 的轮次模型会声称看不到结果）。
func TestBuildInputKeepsToolResults(t *testing.T) {
	withoutInjectedPrompt(t)
	items := buildInput(&adapter.NativeRequest{Messages: []adapter.ChatMessage{
		{Role: "user", Content: "查一下"},
		{Role: "assistant", Content: "", ToolCalls: []adapter.ToolCall{{Name: "search", Arguments: `{"q":"x"}`}}},
		{Role: "tool", Content: "结果: 42"},
		{Role: "user", Content: "结论？"},
	}})
	folded := itemText(t, items[0])
	if !strings.Contains(folded, "Assistant[tool_call]: search") {
		t.Fatalf("历史里缺少工具调用记录: %s", dump(items))
	}
	if !strings.Contains(folded, "[已执行工具的结果]") || !strings.Contains(folded, "- search: 结果: 42") {
		t.Fatalf("工具结果没有单独成块并署名: %s", dump(items))
	}
}

func TestParseCredential(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOAI string
		wantSes string
	}{
		{"raw jwt", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig", ""},
		{"cookie header", "oai-did=abc; prism_oai_access_token=OAI; prism_session_token=SES; other=1", "OAI", "SES"},
		{"cookie editor export", `{"url":"https://prism.openai.com","cookies":[{"name":"oai-did","value":"abc"},{"name":"prism_oai_access_token","value":"OAI2"},{"name":"prism_session_token","value":"SES2"}]}`, "OAI2", "SES2"},
		{"json fields", `{"access_token":"OAI3","session_token":"SES3"}`, "OAI3", "SES3"},
		{"blank", "   ", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCredential(tc.raw)
			if got.OAI != tc.wantOAI || got.Session != tc.wantSes {
				t.Fatalf("parseCredential(%q) = %+v, want oai=%q session=%q", tc.raw, got, tc.wantOAI, tc.wantSes)
			}
		})
	}
}

// completed payload 形状来自抓包：output[].content[].type=output_text。
func TestExtractText(t *testing.T) {
	payload := map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "output_text", "text": "内部推理"}}},
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "第一段。"},
			map[string]any{"type": "output_text", "text": "第二段。"},
		}},
	}}
	if got := extractText(payload); got != "第一段。第二段。" {
		t.Fatalf("extractText = %q", got)
	}
	if got := extractText(nil); got != "" {
		t.Fatalf("nil payload should yield empty text, got %q", got)
	}
}

// completed payload 里的 reasoning 项形状来自线上抓包（PRISM_LOG_PAYLOAD）：
// {"type":"reasoning","summary":[{"type":"summary_text","text":...}],"thoughtDurationMs":8157}。
func TestExtractReasoning(t *testing.T) {
	payload := map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "先想第一段。"},
			map[string]any{"type": "summary_text", "text": "再想第二段。"},
		}},
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "答案。"},
		}},
	}}
	if got := extractReasoning(payload); got != "先想第一段。\n再想第二段。" {
		t.Fatalf("extractReasoning = %q", got)
	}
	// 纯 message 轮（线上 ~84% 的形状）没有思考摘要。
	noReasoning := map[string]any{"output": []any{
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "答案。"},
		}},
	}}
	if got := extractReasoning(noReasoning); got != "" {
		t.Fatalf("message-only payload should yield empty reasoning, got %q", got)
	}
	if got := extractReasoning(nil); got != "" {
		t.Fatalf("nil payload should yield empty reasoning, got %q", got)
	}
}

// 上游 completed payload 带 reasoning 项时（线上抓包形状），Stream 必须把思考摘要
// 作为 Thinking 事件在正文之前发出——api 层靠它产出 reasoning_content 帧并计入 completion。
func TestStreamEmitsThinkingBeforeText(t *testing.T) {
	m := newHardeningMock()
	defer m.srv.Close()
	m.statusJSON = `{"status":"completed","request_id":"r1","turn_state":{"prompt":"hi"},"response":{"payload":{"output":[` +
		`{"id":"reasoning_1","type":"reasoning","summary":[{"type":"summary_text","text":"先想想。"}],"thoughtDurationMs":100},` +
		`{"type":"message","content":[{"type":"output_text","text":"答案。"}]}]}}}`
	c := m.newClient()
	nr := &adapter.NativeRequest{
		Model:    DefaultModel,
		Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}},
	}
	var thinking, text strings.Builder
	var order []string
	if err := c.Stream(context.Background(), nr, func(ev adapter.Event) bool {
		if ev.Thinking != nil {
			thinking.WriteString(ev.Thinking.Text)
			order = append(order, "thinking")
		}
		if ev.Text != "" {
			text.WriteString(ev.Text)
			order = append(order, "text")
		}
		return true
	}); err != nil {
		t.Fatalf("Stream err: %v", err)
	}
	if thinking.String() != "先想想。" {
		t.Fatalf("thinking = %q, want 思考摘要", thinking.String())
	}
	if text.String() != "答案。" {
		t.Fatalf("text = %q, want 正文", text.String())
	}
	if len(order) != 2 || order[0] != "thinking" || order[1] != "text" {
		t.Fatalf("event order = %v, want [thinking text]", order)
	}
}

func itemRole(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("item is not an object: %#v", v)
	}
	role, _ := m["role"].(string)
	return role
}

func itemText(t *testing.T, v any) string {
	t.Helper()
	m, _ := v.(map[string]any)
	contents, _ := m["content"].([]any)
	var sb strings.Builder
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		sb.WriteString(cm["text"].(string))
	}
	return sb.String()
}

func dump(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
