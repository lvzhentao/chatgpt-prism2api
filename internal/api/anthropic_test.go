package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-2api/internal/config"
)

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		MockMode:      true,
		MockAddr:      "127.0.0.1:0",
		CredentialDir: dir,
		APIKeyAuth:    "sk-test-key",
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("failed to create test server: %v", err)
	}
	return s
}

func TestAnthropicCountTokens(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()

	body := `{
		"model": "claude-opus-4-8",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hello, count my tokens!"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AnthropicCountTokensResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive input_tokens, got %d", resp.InputTokens)
	}
}

func TestAnthropicIdentityProbe(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()

	body := `{
		"model": "claude-opus-4-8",
		"messages": [
			{"role": "user", "content": "Who are you?"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AnthropicMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Content) == 0 || !strings.Contains(resp.Content[0].Text, "Claude") {
		t.Fatalf("unexpected probe response: %+v", resp)
	}
}

func TestAnthropicAuthRequired(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()

	body := `{
		"model": "claude-opus-4-8",
		"messages": [
			{"role": "user", "content": "Testing without key"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", w.Code)
	}
}

func TestAnthropicRequestConversion(t *testing.T) {
	t.Setenv("WEB2API_MAX_EFFORT", "max") // 放开上限，只测转换映射
	raw := `{
		"model": "claude-3-7-sonnet-20250219",
		"max_tokens": 1024,
		"thinking": {"type": "enabled", "budget_tokens": 20000},
		"system": "System instructions here",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Run a tool"}]},
			{
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "call_123", "name": "get_weather", "input": {"city": "Tokyo"}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "call_123", "content": "Sunny 25C"}
				]
			}
		],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get current weather",
				"input_schema": {
					"type": "object",
					"properties": {
						"city": {"type": "string"}
					},
					"required": ["city"]
				}
			}
		]
	}`

	var anthropicReq AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(raw), &anthropicReq); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	chatReq, err := AnthropicToOpenAIRequest(&anthropicReq, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if chatReq.ReasoningEffort != "high" {
		t.Errorf("expected reasoning_effort high, got %s", chatReq.ReasoningEffort)
	}
	if len(chatReq.Tools) != 1 || chatReq.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools conversion mismatch: %+v", chatReq.Tools)
	}

	// Verify messages structure: system + user + assistant (with tool_calls) + tool
	if len(chatReq.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "system" {
		t.Errorf("msg 0 role mismatch: %s", chatReq.Messages[0].Role)
	}
	if chatReq.Messages[1].Role != "user" {
		t.Errorf("msg 1 role mismatch: %s", chatReq.Messages[1].Role)
	}
	if chatReq.Messages[2].Role != "assistant" || len(chatReq.Messages[2].ToolCalls) != 1 {
		t.Errorf("msg 2 tool_calls mismatch: %+v", chatReq.Messages[2])
	}
	if chatReq.Messages[3].Role != "tool" || chatReq.Messages[3].ToolCallID != "call_123" {
		t.Errorf("msg 3 tool result mismatch: %+v", chatReq.Messages[3])
	}
}

func TestClaudeCodeRequestConversion(t *testing.T) {
	t.Setenv("WEB2API_MAX_EFFORT", "max") // 放开上限，只测转换映射
	raw := `{
		"model": "claude-3-7-sonnet-20250219",
		"max_tokens": 4096,
		"betas": ["claude-code-20250219", "interleaved-thinking-2025-05-14", "context-1m-2025-08-07"],
		"output_config": {"effort": "medium"},
		"system": [
			{"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "Context: git repository clean"}
		],
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Please check files", "cache_control": {"type": "ephemeral"}}
				]
			}
		],
		"tools": [
			{
				"name": "Bash",
				"description": "Execute bash commands",
				"input_schema": {
					"type": "object",
					"properties": {"command": {"type": "string"}},
					"required": ["command"]
				},
				"cache_control": {"type": "ephemeral"}
			}
		]
	}`

	var req AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal Claude Code request error: %v", err)
	}

	if len(req.Betas) != 3 {
		t.Errorf("expected 3 betas, got %d", len(req.Betas))
	}
	customMap := map[string]string{
		"claude-3-7-sonnet": "claude-4.5-sonnet",
	}
	chatReq, err := AnthropicToOpenAIRequest(&req, customMap, "claude-4.5-sonnet")
	if err != nil {
		t.Fatalf("convert Claude Code request error: %v", err)
	}

	if chatReq.ReasoningEffort != "medium" {
		t.Errorf("expected reasoning_effort medium from output_config, got %s", chatReq.ReasoningEffort)
	}

	if chatReq.Model != "claude-4.5-sonnet" {
		t.Errorf("expected mapped model claude-4.5-sonnet, got %s", chatReq.Model)
	}

	tokens := EstimateAnthropicTokens(&req)
	if tokens <= 0 {
		t.Errorf("expected positive estimated tokens, got %d", tokens)
	}
}
func TestCleanAnthropicModelNameFallbacks(t *testing.T) {
	cases := []struct {
		input     string
		customMap map[string]string
		defModel  string
		expected  string
	}{
		{"claude-3-7-sonnet-20250219", nil, "claude-4.5-sonnet", "claude-3-7-sonnet-20250219"},
		{"claude-3-5-sonnet-20241022", nil, "claude-4.5-sonnet", "claude-3-5-sonnet-20241022"},
		{"claude-3-5-haiku-20241022", map[string]string{"claude-3-5-haiku": "claude-4.5-sonnet"}, "claude-4.5-sonnet", "claude-4.5-sonnet"},
		{"claude-opus-4-8", nil, "claude-4.5-sonnet", "claude-opus-4-8"},
		{"my-custom-model", map[string]string{"my-custom-model": "claude-opus-4-8-high"}, "claude-4.5-sonnet", "claude-opus-4-8-high"},
		{"gpt-4o", nil, "claude-4.5-sonnet", "gpt-4o"},
		{"", nil, "claude-opus-4-8-high", "claude-opus-4-8-high"},
	}

	for _, c := range cases {
		got := CleanAnthropicModelName(c.input, c.customMap, c.defModel)
		if got != c.expected {
			t.Errorf("CleanAnthropicModelName(%q) = %q, expected %q", c.input, got, c.expected)
		}
	}
}
