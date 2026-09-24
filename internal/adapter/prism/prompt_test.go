package prism

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

// 上游服务端自带一套指令（真机：只发一条 user 时模型自述"我是 Codex"）。
// 替换它靠合并后的**单条** system item：注入文案在最前，调用方指令随后（P0-1：
// 多条 system 并存时最后一条会盖掉前面的，多轮场景调用方 system 会被历史吃掉）。
func TestSystemPromptInjectedBeforeCallerSystem(t *testing.T) {
	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{
		{Role: "system", Content: "调用方的指令"},
		{Role: "user", Content: "问题"},
	}}
	items := buildInput(nr)
	if len(items) != 2 {
		t.Fatalf("want 2 items (合并 system + user), got %d: %s", len(items), dump(items))
	}
	if itemRole(t, items[0]) != "system" {
		t.Fatalf("合并的前文必须是 system 角色（不带 tools 时上游才采信），got %q", itemRole(t, items[0]))
	}
	merged := itemText(t, items[0])
	if !strings.HasPrefix(merged, DefaultSystemPrompt) {
		t.Fatalf("注入的默认指令必须在合并块最前，got %q", truncate(merged, 80))
	}
	if !strings.Contains(merged, "\n\n[客户端指令]\n调用方的指令") {
		t.Fatalf("调用方 system 应在 [客户端指令] 段内，got %q", truncate(merged, 200))
	}
	if last := items[1]; itemRole(t, last) != "user" || itemText(t, last) != "问题" {
		t.Fatalf("最后一条必须是本轮 user 请求，got %s", dump(items))
	}
}

func TestSystemPromptEnvOverrideAndOff(t *testing.T) {
	nr := func() *adapter.NativeRequest {
		return &adapter.NativeRequest{Messages: []adapter.ChatMessage{
			{Role: "system", Content: "调用方的指令"},
			{Role: "user", Content: "问题"},
		}}
	}

	t.Setenv("PRISM_SYSTEM_PROMPT", `第一行\n第二行`)
	items := buildInput(nr())
	want := "第一行\n第二行\n\n[客户端指令]\n调用方的指令"
	if got := itemText(t, items[0]); got != want {
		t.Fatalf("PRISM_SYSTEM_PROMPT 应覆盖默认指令（并把 \\n 还原成换行），got %q", got)
	}

	t.Setenv("PRISM_SYSTEM_PROMPT", "off")
	items = buildInput(nr())
	if len(items) != 2 {
		t.Fatalf("off 时应完全不注入，got %d items: %s", len(items), dump(items))
	}
	if got := itemText(t, items[0]); got != "[客户端指令]\n调用方的指令" {
		t.Fatalf("off 时首项应是调用方自己的 system（带标签），got %q", got)
	}
}

func TestSystemPromptFileOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("  文件里的指令  "), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_SYSTEM_PROMPT_FILE", path)
	items := buildInput(&adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "问题"}}})
	if got := itemText(t, items[0]); got != "文件里的指令" {
		t.Fatalf("PRISM_SYSTEM_PROMPT_FILE 应作为注入内容（去空白），got %q", got)
	}

	t.Setenv("PRISM_SYSTEM_PROMPT_FILE", filepath.Join(t.TempDir(), "missing.txt"))
	items = buildInput(&adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "问题"}}})
	if got := itemText(t, items[0]); got != DefaultSystemPrompt {
		t.Fatalf("文件读不到时应退回默认指令，got %q", truncate(got, 80))
	}
}

// 声明了 tools 时上游不采信 system（真机 §4.7），注入的指令必须并进 user 消息，
// 且排在块首——否则等于没注入。
func TestToolsPathInjectsPromptIntoUserMessage(t *testing.T) {
	nr := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{
			{Role: "system", Content: "调用方的指令"},
			{Role: "user", Content: "查天气"},
		},
		Tools: []adapter.ToolDef{{Name: "get_weather"}},
	}
	items := buildInput(nr)
	if len(items) != 1 || itemRole(t, items[0]) != "user" {
		t.Fatalf("带 tools 时只能有一条 user item（上游忽略 system），got %s", dump(items))
	}
	text := itemText(t, items[0])
	if !strings.HasPrefix(text, "[系统指令]\n"+DefaultSystemPrompt) {
		t.Fatalf("注入的指令必须排在 user 消息最前，got:\n%s", truncate(text, 300))
	}
	for _, want := range []string{"[客户端指令]\n调用方的指令", toolCallOpen, "[用户当前消息]\n查天气"} {
		if !strings.Contains(text, want) {
			t.Errorf("工具链路的 user 消息缺少 %q:\n%s", want, truncate(text, 400))
		}
	}
}

// nr.SystemPrompt 非空时直接用它（api 层热配置的解析结果）；只有空时才走 env/默认。
func TestSystemPromptForUsesConfiguredFirst(t *testing.T) {
	nr := func() *adapter.NativeRequest {
		return &adapter.NativeRequest{Messages: []adapter.ChatMessage{{Role: "user", Content: "hi"}}}
	}
	t.Setenv("PRISM_SYSTEM_PROMPT", "off")
	n := nr()
	n.SystemPrompt = "热配置文案"
	items := buildInput(n)
	if got := itemText(t, items[0]); got != "热配置文案" {
		t.Fatalf("nr.SystemPrompt 非空应优先于 env=off，got %q", got)
	}
}
