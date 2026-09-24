package api

import "testing"

func TestApplyOpenAIThinkingDefaults(t *testing.T) {
	// thinking 变体 → reasoning_effort 补全 + 模型名还原
	// （显式变体不应用默认 effort 上限：用户选 -thinking-high/-max 就是明确意图）
	req := &ChatCompletionRequest{Model: "claude-opus-4-8-thinking-high"}
	applyOpenAIThinkingDefaults(req)
	if req.Model != "claude-opus-4-8-thinking" || req.ReasoningEffort != "high" {
		t.Fatalf("thinking variant: model=%q effort=%q", req.Model, req.ReasoningEffort)
	}

	// 等级变体 → effort
	req = &ChatCompletionRequest{Model: "claude-opus-4-8-low"}
	applyOpenAIThinkingDefaults(req)
	if req.Model != "claude-opus-4-8" || req.ReasoningEffort != "low" {
		t.Fatalf("low variant: model=%q effort=%q", req.Model, req.ReasoningEffort)
	}

	// 显式 reasoning_effort 优先，模型名不动
	req = &ChatCompletionRequest{Model: "claude-opus-4-8-max", ReasoningEffort: "medium"}
	applyOpenAIThinkingDefaults(req)
	if req.Model != "claude-opus-4-8-max" || req.ReasoningEffort != "medium" {
		t.Fatalf("explicit effort: model=%q effort=%q", req.Model, req.ReasoningEffort)
	}

	// 裸名无思考 → 保持
	req = &ChatCompletionRequest{Model: "claude-opus-4-8"}
	applyOpenAIThinkingDefaults(req)
	if req.Model != "claude-opus-4-8" || req.ReasoningEffort != "" {
		t.Fatalf("bare model: model=%q effort=%q", req.Model, req.ReasoningEffort)
	}

	// max_completion_tokens 兼容
	mt := 12345
	req = &ChatCompletionRequest{Model: "claude-opus-4-8", MaxCompletionTokens: &mt}
	applyOpenAIThinkingDefaults(req)
	if req.MaxTokens == nil || *req.MaxTokens != 12345 {
		t.Fatalf("max_completion_tokens not merged: %v", req.MaxTokens)
	}
}
