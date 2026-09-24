package api

import (
	"context"
	"encoding/json"
	"testing"

	"prism-2api/internal/adapter/prism"
	"prism-2api/internal/admin"
)

// toAdapterRequest 必须把调用方解析好的注入文本原样带进 kernel-neutral 请求。
// 回归保护：字段漏传 = 线上注入静默失效，且 buildInput 侧看不出来。
func TestToAdapterRequestCarriesSystemPrompt(t *testing.T) {
	req := &ChatCompletionRequest{Model: "m", Messages: []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	}}
	out := toAdapterRequest(context.Background(), req, "注入文本")
	if out.SystemPrompt != "注入文本" {
		t.Fatalf("SystemPrompt 漏传，got %q", out.SystemPrompt)
	}
	// 注：不调 MapChat（全局 adapter 未绑定，见 adapter.Bind；绑定由 cmd/server 做）。
}

// promptText 的解析顺序：off 关、管理端文案优先、空回内置默认、无 runtime 回内置默认。
func TestPromptTextResolution(t *testing.T) {
	var nilServer *Server
	if got := nilServer.promptText(); got != prism.DefaultSystemPrompt {
		t.Fatalf("nil server 应回内置默认，got %q", got[:20])
	}
	s := &Server{runtime: admin.Default("https://x", "https://y", "", "127.0.0.1:0", true)}
	if got := s.promptText(); got != prism.DefaultSystemPrompt {
		t.Fatalf("默认应回内置默认，got %q", got[:20])
	}
	if _, _, err := s.runtime.Update(map[string]any{"system_prompt": "自定义"}); err != nil {
		t.Fatal(err)
	}
	if got := s.promptText(); got != "自定义" {
		t.Fatalf("管理端文案应优先，got %q", got)
	}
	if _, _, err := s.runtime.Update(map[string]any{"system_prompt_mode": "off"}); err != nil {
		t.Fatal(err)
	}
	if got := s.promptText(); got != "" {
		t.Fatalf("off 应不注入，got %q", got)
	}
}

// api 侧默认文案必须与 adapter 侧保持一致，否则线上默认注入的和单测断言的不是同一份。
func TestDefaultPromptMatchesAdapter(t *testing.T) {
	if defaultInjectedPrompt != prism.DefaultSystemPrompt {
		t.Fatal("defaultInjectedPrompt 与 prism.DefaultSystemPrompt 不一致：改 adapter 文案时同步改 api/prompt.go")
	}
}
