package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
	"prism-2api/internal/adapter/prism"
	"prism-2api/internal/auth"
	"prism-2api/internal/pool"
)

// developer 是 Responses 里 system 的新名字，丢了就等于丢顶层指令。
func TestResponsesDeveloperRoleBecomesSystem(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Input = json.RawMessage(`[
		{"type":"message","role":"developer","content":"Never edit files without asking."},
		{"type":"message","role":"user","content":"hi"}
	]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" {
		t.Fatalf("developer 未映射成 system: %+v", chat.Messages)
	}
}

// developer 的 content 也可能是 parts 数组，映射后同样要摊平。
func TestResponsesDeveloperRoleWithParts(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Input = json.RawMessage(`[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"码农模式"}]},
		{"type":"message","role":"user","content":"go"}
	]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" {
		t.Fatalf("developer 未映射成 system: %+v", chat.Messages)
	}
	if got := contentText(chat.Messages[0].Content); got != "码农模式" {
		t.Fatalf("developer 正文 = %q", got)
	}
}

// reasoning 条目无 role，跳过即可（加密推理只有上游能解），但不能因此丢别的条目。
func TestResponsesSkipsReasoningItems(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Input = json.RawMessage(`[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking…"}],"encrypted_content":"gAAAA"},
		{"type":"message","role":"user","content":"go"}
	]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || contentText(chat.Messages[0].Content) != "go" {
		t.Fatalf("reasoning 干扰了其它条目: %+v", chat.Messages)
	}
}

// Codex CLI 把 apply_patch 这类工具发成 freeform 的 type:"custom"（grammar，不是 JSON schema）。
// 我们没有原生 tools 通道（靠提示词仿真），所以要把它描述成「单 input 字符串参数」，
// 回程再还原成 custom_tool_call/{input} —— 否则 Codex 认不出这个项，补丁工具链就断在这里。
func TestResponsesCustomToolDescribedAsSingleInput(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Tools = []responsesTool{
		{Type: "custom", Name: "apply_patch", Description: "Apply a freeform patch"},
		{Type: "function", Name: "shell", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
	}
	req.Input = json.RawMessage(`[{"type":"message","role":"user","content":"go"}]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 2 {
		t.Fatalf("工具数 = %d, want 2（custom 也要下发）", len(chat.Tools))
	}
	var custom, fn *Tool
	for i := range chat.Tools {
		switch chat.Tools[i].Function.Name {
		case "apply_patch":
			custom = &chat.Tools[i]
		case "shell":
			fn = &chat.Tools[i]
		}
	}
	if custom == nil || fn == nil {
		t.Fatalf("两个工具都要在: %+v", chat.Tools)
	}
	// custom → 单 input 字符串参数
	var schema map[string]any
	if err := json.Unmarshal(custom.Function.Parameters, &schema); err != nil {
		t.Fatalf("custom 的参数不是合法 JSON schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Fatalf("custom 工具应描述成单 input 参数，得到 %s", custom.Function.Parameters)
	}
	// 普通 function 的参数原样保留
	if !strings.Contains(string(fn.Function.Parameters), `"command"`) {
		t.Fatalf("function 工具的参数被改坏了: %s", fn.Function.Parameters)
	}
	if got := customToolNames(req.Tools); !got["apply_patch"] || got["shell"] {
		t.Fatalf("customToolNames = %v，只应含 apply_patch", got)
	}
}

// 回程：custom 工具要发成 custom_tool_call + input，而不是 function_call + arguments。
func TestResponsesCustomToolCallOutputShape(t *testing.T) {
	chatResp := &struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}{Model: "m"}
	chatResp.Choices = append(chatResp.Choices, struct {
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{FinishReason: "tool_calls"})
	chatResp.Choices[0].Message.ToolCalls = []ToolCall{
		{ID: "call_1", Type: "function", Function: ToolCallFunction{
			Name: "apply_patch", Arguments: `{"input":"*** Begin Patch\n*** End Patch\n"}`}},
		{ID: "call_2", Type: "function", Function: ToolCallFunction{
			Name: "shell", Arguments: `{"command":"ls"}`}},
	}

	obj := openaiResponseFromChat(chatResp, map[string]bool{"apply_patch": true})
	if len(obj.Output) != 2 {
		t.Fatalf("output 项数 = %d, want 2", len(obj.Output))
	}
	first, second := obj.Output[0], obj.Output[1]
	if first.Type != "custom_tool_call" || first.Name != "apply_patch" {
		t.Fatalf("第 1 项应是 custom_tool_call/apply_patch，得到 %+v", first)
	}
	if !strings.Contains(first.Input, "Begin Patch") || first.Arguments != "" {
		t.Fatalf("custom 项应带 input、不带 arguments，得到 %+v", first)
	}
	if second.Type != "function_call" || !strings.Contains(second.Arguments, "ls") {
		t.Fatalf("第 2 项应保持 function_call + arguments，得到 %+v", second)
	}
}

// bindMockAdapter 绑定内置 mock 站点：测试进程里 adapter.Default 默认是 nil，
// 账号池造客户端要用它（cmd/server 在 boot 时绑定，测试里没有）。
func bindMockAdapter(t *testing.T) {
	t.Helper()
	prev := adapter.Default
	adapter.Bind(prism.New(prism.Config{Mock: true}))
	t.Cleanup(func() { adapter.Bind(prev) })
}

// testAccount 在 mock 模式服务器里放一个已登录账号并返回它；把 Client 换成脚本客户端
// 就能驱动「内核 → 站点适配器」这一跳。
func testAccount(t *testing.T, s *Server) *pool.Account {
	t.Helper()
	if err := s.pool.SetToken("default", &auth.Token{AccessToken: "upstream-credential"}); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	accs := s.pool.Accounts()
	if len(accs) == 0 {
		t.Fatal("池里没有账号")
	}
	return accs[0]
}

// responsesPOST 打一发 /v1/responses（带网关 Key）。
func responsesPOST(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// scriptedToolClient 按脚本吐事件，用来在 mock 模式下驱动一条工具调用。
type scriptedToolClient struct {
	adapter.Client
	events []adapter.Event
}

func (c *scriptedToolClient) Stream(_ context.Context, _ *adapter.NativeRequest, emit func(adapter.Event) bool) error {
	for _, ev := range c.events {
		if !emit(ev) {
			return nil
		}
	}
	return nil
}

// freeform（type:"custom"）工具的整条链路：客户端声明 → 上游调用 → 回程 custom_tool_call。
// 关键是参数必须原样穿过工具门（见 mapCursorToolCall 的「声明名直通」）：apply_patch 这类
// 名字会撞上 Cursor 内置名的参数重写，参数被剪光时 Codex 收到的补丁就是空的。
func TestResponsesCustomToolStreamsCustomToolCall(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	acc := testAccount(t, s)
	acc.Client = &scriptedToolClient{Client: acc.Client, events: []adapter.Event{
		{ToolCall: &adapter.StreamedToolCall{
			ToolCallID: "call_1", Name: "apply_patch",
			RawArgs: `{"input":"*** Begin Patch\n*** End Patch\n"}`}},
		{Ended: true},
	}}

	w := responsesPOST(t, s, `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"patch it"}],`+
		`"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"type":"custom_tool_call"`,
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		`"input":"*** Begin Patch\n*** End Patch\n"`,
		"sequence_number",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("事件流缺 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"type":"function_call"`) {
		t.Errorf("custom 工具不该回成 function_call:\n%s", body)
	}
	if strings.Contains(body, `"input":"{}"`) {
		t.Errorf("freeform 参数被工具门剪空了:\n%s", body)
	}
}

// failingStreamClient 让「内核 → 站点适配器」这一跳直接失败（预热超预算 / 沙箱劣化），
// 用来验证失败回合在 /v1/responses 上的终态。
type failingStreamClient struct {
	adapter.Client
	err error
}

func (c *failingStreamClient) Stream(context.Context, *adapter.NativeRequest, func(adapter.Event) bool) error {
	return c.err
}

// 失败的一轮在 /v1/responses 流式里必须是 response.failed，**不能**把错误文案
// 当成助手正文 delivered（那样 agent 会把错误当上下文继续往下跑）。
// 非流式路径由 handleChat 直接回错误状态，不走这里。
func TestResponsesStreamTurnFailureIsFailedEvent(t *testing.T) {
	bindMockAdapter(t)
	s := setupTestServer(t)
	acc := testAccount(t, s)
	acc.Client = &failingStreamClient{
		Client: acc.Client,
		err:    errors.New("prism: upstream sandbox degraded: new backend: context deadline exceeded"),
	}

	w := responsesPOST(t, s, `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"go"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("流式已开 SSE，状态码仍是 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var events []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
		}
	}
	joined := strings.Join(events, " ")
	if !strings.Contains(joined, "response.failed") {
		t.Fatalf("失败回合应发 response.failed，实际事件: %s", joined)
	}
	if strings.Contains(joined, "response.completed") {
		t.Errorf("失败回合并发 response.completed 是错的: %s", joined)
	}
	// 错误只能是 failed 事件里的 error，绝不能作为助手正文下发。
	if strings.Contains(w.Body.String(), `"type":"output_text"`) {
		t.Errorf("失败回合下发了助手正文:\n%s", w.Body.String())
	}
}

// custom_tool_call_output 回灌后要出现在 chat 请求的 tool 消息里，且 call_id 对应。
// 否则第二轮续接时模型看不到工具结果，上下文断裂。
func TestResponsesCustomToolCallOutputRoundTrip(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Input = json.RawMessage(`[
		{"type":"custom_tool_call_output","call_id":"call_7","output":"patched ok"},
		{"type":"function_call_output","call_id":"call_8","output":[{"type":"input_text","text":"done"}]}
	]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("消息数 = %d, want 2（两类 output 都要回灌）: %+v", len(chat.Messages), chat.Messages)
	}
	for i, want := range []struct {
		callID string
		text   string
	}{
		{"call_7", "patched ok"},
		{"call_8", "done"},
	} {
		m := chat.Messages[i]
		if m.Role != "tool" || m.ToolCallID != want.callID {
			t.Fatalf("第 %d 条应是 tool/%s，得到 %+v", i, want.callID, m)
		}
		var got string
		if err := json.Unmarshal(m.Content, &got); err != nil {
			t.Fatalf("第 %d 条 content 不是字符串: %s", i, m.Content)
		}
		if got != want.text {
			t.Fatalf("第 %d 条内容 = %q, want %q", i, got, want.text)
		}
	}
}

// custom_tool_call 续接的 arguments 必须是 {"input":"..."} 形状，
// 与出站描述的单 input 参数一致，否则回灌后对不上。
func TestResponsesCustomToolCallRoundTrip(t *testing.T) {
	req := &ResponsesRequest{Model: "m"}
	req.Input = json.RawMessage(`[
		{"type":"custom_tool_call","call_id":"call_9","name":"apply_patch","input":"*** Begin Patch\n*** End Patch\n"},
		{"type":"custom_tool_call","call_id":"call_10","name":"apply_patch","input":{"path":"a.go","diff":"x"}}
	]`)
	chat, err := responsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("消息数 = %d, want 2: %+v", len(chat.Messages), chat.Messages)
	}
	for i, wantInput := range []string{"*** Begin Patch\n*** End Patch\n", `{"diff":"x","path":"a.go"}`} {
		m := chat.Messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) != 1 {
			t.Fatalf("第 %d 条应是带单 tool_call 的 assistant，得到 %+v", i, m)
		}
		tc := m.ToolCalls[0]
		if tc.Function.Name != "apply_patch" {
			t.Fatalf("第 %d 条工具名 = %q, want apply_patch", i, tc.Function.Name)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			t.Fatalf("第 %d 条 arguments 不是 JSON: %s", i, tc.Function.Arguments)
		}
		got, _ := args["input"].(string)
		if i == 0 && got != wantInput {
			t.Fatalf("字符串 input 应原样包进 arguments，得到 %q", got)
		}
		if i == 1 {
			var gotObj, wantObj map[string]any
			if err := json.Unmarshal([]byte(got), &gotObj); err != nil {
				t.Fatalf("对象 input 应归一成 JSON 字符串再包进，得到 %q", got)
			}
			if err := json.Unmarshal([]byte(wantInput), &wantObj); err != nil {
				t.Fatal(err)
			}
			if len(gotObj) != len(wantObj) || gotObj["path"] != wantObj["path"] || gotObj["diff"] != wantObj["diff"] {
				t.Fatalf("对象 input 归一后对不上，得到 %q", got)
			}
		}
	}
}

// type:"custom" 声明按嵌套 function 形态写名字时也要收进 custom 集合，
// 否则回程还原不出 custom_tool_call。
func TestResponsesCustomToolNamesNestedFunction(t *testing.T) {
	tools := []responsesTool{
		{Type: "custom", Function: &struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: "apply_patch"}},
	}
	if got := customToolNames(tools); !got["apply_patch"] {
		t.Fatalf("嵌套 function 形态的名字应被收集，得到 %v", got)
	}
}
