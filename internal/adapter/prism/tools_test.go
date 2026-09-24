package prism

import (
	"encoding/json"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

func TestExtractToolCallsFromText(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantNames []string
		wantArgs  []string
		wantText  string
	}{
		{
			name:      "单个调用 + 前缀说明",
			in:        "我先查天气。\n<tool_call>\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"北京\"}}\n</tool_call>",
			wantNames: []string{"get_weather"},
			wantArgs:  []string{`{"city":"北京"}`},
			wantText:  "我先查天气。",
		},
		{
			name: "并行两个调用",
			in: "<tool_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"北京\"}}</tool_call>\n" +
				"<tool_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"上海\"}}</tool_call>",
			wantNames: []string{"get_weather", "get_weather"},
			wantArgs:  []string{`{"city":"北京"}`, `{"city":"上海"}`},
			wantText:  "",
		},
		{
			name:      "arguments 是 JSON 字符串",
			in:        "<tool_call>{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.tex\\\"}\"}</tool_call>",
			wantNames: []string{"read_file"},
			wantArgs:  []string{`{"path":"a.tex"}`},
			wantText:  "",
		},
		{
			name:      "arguments 缺省补空对象",
			in:        "<tool_call>{\"name\":\"list_files\"}</tool_call>",
			wantNames: []string{"list_files"},
			wantArgs:  []string{"{}"},
			wantText:  "",
		},
		{
			name:     "损坏的 JSON 原样保留",
			in:       "<tool_call>{oops}</tool_call>",
			wantText: "<tool_call>{oops}</tool_call>",
		},
		{
			name:     "没有调用块时正文不变",
			in:       "北京今天多云，22℃。",
			wantText: "北京今天多云，22℃。",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, text := extractToolCallsFromText(c.in)
			if len(calls) != len(c.wantNames) {
				t.Fatalf("调用数 = %d, want %d (%+v)", len(calls), len(c.wantNames), calls)
			}
			for i, call := range calls {
				if call.Name != c.wantNames[i] {
					t.Errorf("calls[%d].Name = %q, want %q", i, call.Name, c.wantNames[i])
				}
				if call.RawArgs != c.wantArgs[i] {
					t.Errorf("calls[%d].RawArgs = %q, want %q", i, call.RawArgs, c.wantArgs[i])
				}
				if !json.Valid([]byte(call.RawArgs)) {
					t.Errorf("calls[%d].RawArgs 不是合法 JSON: %q", i, call.RawArgs)
				}
				if call.ToolCallID == "" {
					t.Errorf("calls[%d] 缺 ToolCallID（客户端要拿它回灌结果）", i)
				}
			}
			if text != c.wantText {
				t.Errorf("剥离后正文 = %q, want %q", text, c.wantText)
			}
		})
	}
}

func TestExtractToolCallsFromTextUniqueIDs(t *testing.T) {
	calls, _ := extractToolCallsFromText(
		"<tool_call>{\"name\":\"a\"}</tool_call><tool_call>{\"name\":\"b\"}</tool_call>")
	if len(calls) != 2 {
		t.Fatalf("调用数 = %d, want 2", len(calls))
	}
	if calls[0].ToolCallID == calls[1].ToolCallID {
		t.Fatalf("两个调用的 ToolCallID 撞了: %q", calls[0].ToolCallID)
	}
}

func TestToolContractListsEveryTool(t *testing.T) {
	nr := &adapter.NativeRequest{Tools: []adapter.ToolDef{
		{Name: "get_weather", Description: "查天气", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "read_file", Description: "读文件"},
	}}
	c := toolContract(nr)
	for _, want := range []string{"get_weather", "查天气", "read_file", "读文件", `{"type":"object"}`, toolCallOpen, toolCallClose} {
		if !strings.Contains(c, want) {
			t.Errorf("工具协议里缺少 %q:\n%s", want, c)
		}
	}
	// 具名示例：示例块必须用真实工具名钉死（抽象占位会导致模型按沙箱习惯改名）。
	if !strings.Contains(c, `"name":"get_weather"`) {
		t.Errorf("协议示例必须用首个真实工具名具名填充:\n%s", c)
	}
}

func TestBuildInputSurfacesExecutedToolResults(t *testing.T) {
	nr := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{
			{Role: "user", Content: "查天气。"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "call_a1", Name: "get_weather", Arguments: `{"city":"北京"}`}}},
			{Role: "tool", ToolCallID: "call_a1", Content: "北京：晴，25℃"},
			{Role: "user", Content: "温度多少？"},
		},
		Tools: []adapter.ToolDef{{Name: "get_weather"}},
	}
	items := buildInput(nr)
	var texts []string
	for _, it := range items {
		m, _ := it.(map[string]any)
		contents, _ := m["content"].([]any)
		for _, cc := range contents {
			cm, _ := cc.(map[string]any)
			texts = append(texts, stringAt(cm, "text"))
		}
	}
	joined := strings.Join(texts, "\n---\n")
	// 结果必须单独成块（真机验证：只放 [对话历史] 里模型会声称看不到）
	if !strings.Contains(joined, "[已执行工具的结果——客户端已在它自己的机器上真实执行，数据如下，直接用]\n- get_weather: 北京：晴，25℃") {
		t.Fatalf("工具结果没有单独成块:\n%s", joined)
	}
	// 历史里保留调用记录，供模型对齐上一步
	if !strings.Contains(joined, `Assistant[tool_call]: get_weather {"city":"北京"}`) {
		t.Errorf("历史里缺少工具调用记录:\n%s", joined)
	}
	// 声明了 tools 时协议说明必须在场
	if !strings.Contains(joined, toolCallOpen) {
		t.Errorf("缺少工具协议说明:\n%s", joined)
	}
}

// 真机（2026-09-17）：请求带 tools 时上游不采信 system 内容，所以工具链路的
// 上下文必须全部走本轮 user 消息，一个 system item 都不能发。
func TestBuildInputToolsPathUsesSingleUserMessage(t *testing.T) {
	withoutInjectedPrompt(t)
	nr := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{
			{Role: "system", Content: "你是简洁助手。"},
			{Role: "user", Content: "查天气。"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "call_a1", Name: "get_weather", Arguments: `{"city":"北京"}`}}},
			{Role: "tool", ToolCallID: "call_a1", Content: "北京：晴，25℃"},
			{Role: "user", Content: "上海呢？"},
		},
		Tools: []adapter.ToolDef{{Name: "get_weather"}},
	}
	items := buildInput(nr)
	if len(items) != 1 || itemRole(t, items[0]) != "user" {
		t.Fatalf("带 tools 时必须只有一条 user item，got %s", dump(items))
	}
	text := itemText(t, items[0])
	for _, want := range []string{"[客户端指令]", "你是简洁助手。", "[对话历史]", "[已执行工具的结果]", "- get_weather: 北京：晴，25℃", toolCallOpen, "[用户当前消息]\n上海呢？"} {
		if !strings.Contains(text, want) {
			t.Errorf("工具链路的 user 消息缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[用户当前消息]\n查天气。") {
		t.Errorf("最后一条 user 才是本轮请求，历史的 user 轮不该出现在 [用户当前消息] 里:\n%s", text)
	}
}

// 强化重试轮：Extra 置位时纠正块必须进唯一 user 消息且带具名示例；未置位时不得出现。
func TestBuildInputAppendsReinforceBlockWhenFlagged(t *testing.T) {
	withoutInjectedPrompt(t)
	mk := func(extra map[string]any) *adapter.NativeRequest {
		return &adapter.NativeRequest{
			Messages: []adapter.ChatMessage{{Role: "user", Content: "调用 Read 工具读取 README.md"}},
			Tools:    []adapter.ToolDef{{Name: "Read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
			Extra:    extra,
		}
	}
	flagged := itemText(t, buildInput(mk(map[string]any{
		"prism_tool_reinforce":     "1",
		"prism_tool_reinforce_bad": "无法读取：`README.md` 不存在。",
	}))[0])
	if !strings.Contains(flagged, "[协议纠正]") || !strings.Contains(flagged, `{"name":"Read","arguments":{"path":"README.md"}}`) {
		t.Fatalf("置位时必须带纠正块与具名示例:\n%s", flagged)
	}
	if !strings.Contains(flagged, "无法读取：`README.md` 不存在。") {
		t.Fatalf("纠正块必须引用违规原文作证据:\n%s", flagged)
	}
	plain := itemText(t, buildInput(mk(nil))[0])
	if strings.Contains(plain, "[协议纠正]") {
		t.Fatalf("未置位时不得出现纠正块:\n%s", plain)
	}
}

// 交付追捞轮：Extra 置位时追捞块必须出现在消息末尾并引用违规原文；未置位时不得出现。
// 覆盖 tools 与无 tools 两条挂载路径（无 tools 时拼进最后一条 user 消息）。
func TestBuildInputAppendsDeliverySalvageBlockWhenFlagged(t *testing.T) {
	withoutInjectedPrompt(t)
	extra := map[string]any{
		"prism_delivery_salvage":     "delivery",
		"prism_delivery_salvage_bad": "已完成精美的鹈鹕骑车单页 index.html。",
	}
	// 无 tools：追捞块拼在最后一条 user 消息里。
	noTools := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{{Role: "user", Content: "做一个鹈鹕骑车的精美的 html"}},
		Extra:    extra,
	}
	items := buildInput(noTools)
	userText := itemText(t, items[len(items)-1])
	if !strings.Contains(userText, "[交付纠正]") || !strings.Contains(userText, "一行不落") {
		t.Fatalf("无 tools 置位时追捞块必须拼进 user 消息:\n%s", userText)
	}
	if !strings.Contains(userText, "已完成精美的鹈鹕骑车单页 index.html。") {
		t.Fatalf("追捞块必须引用上一轮原文:\n%s", userText)
	}
	if !strings.Contains(userText, "做一个鹈鹕骑车的精美的 html") {
		t.Fatalf("本轮用户消息必须保留:\n%s", userText)
	}
	// 带 tools：追捞块进唯一 user 消息且与协议强化块互斥（reinforce 未置位时不出现）。
	withTools := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{{Role: "user", Content: "做个页面"}},
		Tools:    []adapter.ToolDef{{Name: "Write", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Extra:    extra,
	}
	toolsText := itemText(t, buildInput(withTools)[0])
	if !strings.Contains(toolsText, "[交付纠正]") {
		t.Fatalf("带 tools 置位时追捞块必须在场:\n%s", toolsText)
	}
	// refusal 模式：文案换成"翻自己环境"的纠正。
	refusal := map[string]any{
		"prism_delivery_salvage":     "refusal",
		"prism_delivery_salvage_bad": "无法读取：`README.md` 不存在。",
	}
	noTools.Extra = refusal
	refusalText := itemText(t, buildInput(noTools)[len(items)-1])
	if !strings.Contains(refusalText, "他自己的机器上") {
		t.Fatalf("refusal 模式文案错误:\n%s", refusalText)
	}
	// 未置位时不得出现。
	noTools.Extra = nil
	plain := itemText(t, buildInput(noTools)[len(items)-1])
	if strings.Contains(plain, "[交付纠正]") {
		t.Fatalf("未置位时不得出现追捞块:\n%s", plain)
	}
}
