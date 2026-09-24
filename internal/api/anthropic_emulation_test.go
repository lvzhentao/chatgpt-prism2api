package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"prism-2api/internal/emulation"
)

func TestThinkingAdaptiveMapsEffort(t *testing.T) {
	raw := `{"model":"claude-opus-4-8","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi there long enough"}]}`
	var req AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	chat, err := AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	// adaptive 无预算上限 → 默认官方低预算语义（low），防上游无限思考
	if chat.ReasoningEffort != "low" {
		t.Fatalf("effort %q", chat.ReasoningEffort)
	}
}

func TestThinkingBudgetMapsEffort(t *testing.T) {
	t.Setenv("WEB2API_MAX_EFFORT", "max") // 放开上限，只测 budget→effort 映射
	cases := []struct {
		budget int
		want   string
	}{
		{500, "low"},
		{2000, "medium"},
		{7999, "medium"},
		{8000, "high"},
		{20000, "high"},
	}
	for _, c := range cases {
		req := AnthropicMessagesRequest{
			Model:    "claude-opus-4-8",
			Thinking: &AnthropicThinkingSpec{Type: "enabled", BudgetTokens: c.budget},
			Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
		chat, err := AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
		if err != nil {
			t.Fatal(err)
		}
		if chat.ReasoningEffort != c.want {
			t.Errorf("budget %d: effort %q, want %q", c.budget, chat.ReasoningEffort, c.want)
		}
	}
}

func TestMaxTokensCapped(t *testing.T) {
	t.Setenv("WEB2API_MAX_TOKENS_CAP", "16000")
	defer t.Setenv("WEB2API_MAX_TOKENS_CAP", "")
	req := AnthropicMessagesRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64000,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	chat, err := AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 16000 {
		t.Fatalf("max_tokens not capped: %v", chat.MaxTokens)
	}
	if chat.ReasoningEffort != "" {
		t.Fatalf("no thinking -> no effort: %q", chat.ReasoningEffort)
	}
	// 低于上限不受影响
	req.MaxTokens = 8000
	chat, err = AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if chat.MaxTokens == nil || *chat.MaxTokens != 8000 {
		t.Fatalf("max_tokens below cap must pass through: %v", chat.MaxTokens)
	}
}

// TestOutputConfigEffortCapped 回归：Claude Code 2026 默认发 output_config.effort="max"
// （Cursor 无限思考，实测 40-181s 纯 thinking），必须被压到 WEB2API_MAX_EFFORT。
func TestOutputConfigEffortCapped(t *testing.T) {
	t.Setenv("WEB2API_MAX_EFFORT", "")
	req := AnthropicMessagesRequest{
		Model:        "claude-opus-4-8",
		OutputConfig: &AnthropicOutputConfig{Effort: "max"},
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	chat, err := AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if chat.ReasoningEffort != "low" {
		t.Fatalf("effort=max must cap to low by default, got %q", chat.ReasoningEffort)
	}
	// 显式上限可调
	t.Setenv("WEB2API_MAX_EFFORT", "high")
	chat, err = AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if chat.ReasoningEffort != "high" {
		t.Fatalf("effort=max with cap=high: got %q", chat.ReasoningEffort)
	}
	// 低于上限的等级原样保留
	req.OutputConfig.Effort = "low"
	t.Setenv("WEB2API_MAX_EFFORT", "high")
	chat, err = AnthropicToOpenAIRequest(&req, nil, "claude-4.5-sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if chat.ReasoningEffort != "low" {
		t.Fatalf("effort=low must stay low: got %q", chat.ReasoningEffort)
	}
}

func TestDocumentBlockExtracted(t *testing.T) {
	req := AnthropicMessagesRequest{
		Model: "claude-opus-4-8",
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"PDF TOKEN 438810"}},{"type":"text","text":"what is in the file"}]`),
		}},
	}
	chat, err := AnthropicToOpenAIRequest(&req, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(chat.Messages)
	if !strings.Contains(string(blob), "438810") {
		t.Fatalf("document text missing: %s", blob)
	}
}

func TestIdentityProbeUsageHasInputTokens(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"Who are you?"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Fatalf("stream missing input_tokens: %s", w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("request-id"), "req_01") {
		t.Fatalf("request-id %q", w.Header().Get("request-id"))
	}
	if !strings.Contains(w.Body.String(), `"id":"msg_01`) {
		t.Fatalf("message id not official: %s", w.Body.String())
	}
	sse := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(sse), "event: message_start") {
		t.Fatalf("first event should be message_start: %s", sse[:min(120, len(sse))])
	}
	if strings.HasPrefix(strings.TrimSpace(sse), ": ping") {
		t.Fatal("comment ping must not precede message_start")
	}
	if !strings.Contains(sse, "event: ping") {
		t.Fatalf("missing official ping: %s", sse)
	}
	if !strings.Contains(sse, `"output_tokens":0`) {
		t.Fatalf("message_start output_tokens must be 0: %s", sse)
	}
	if strings.Contains(strings.ToLower(w.Header().Get("anthropic-organization-id")), "cursor") {
		t.Fatalf("org id leaked cursor: %q", w.Header().Get("anthropic-organization-id"))
	}
	if w.Header().Get("anthropic-ratelimit-unified-5h-status") != "allowed" {
		t.Fatalf("missing unified ratelimit: %q", w.Header().Get("anthropic-ratelimit-unified-5h-status"))
	}
}

func TestCodeExecutionInterceptPrintsHELLOCHECK(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"tools":[{"type":"code_execution_20250522","name":"code_execution"}],"messages":[{"role":"user","content":"Write and execute a Python script that prints 'HELLO_CHECK'. Only use the code execution tool, nothing else."}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"type":"server_tool_use"`) || !strings.Contains(out, `code_execution_tool_result`) {
		t.Fatalf("missing server tool blocks: %s", out)
	}
	if !strings.Contains(out, "HELLO_CHECK") {
		t.Fatalf("stdout missing HELLO_CHECK: %s", out)
	}
}

func TestWebSearchInterceptEmitsOfficialBlocks(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"tool","name":"web_search"},"messages":[{"role":"user","content":"Perform a web search for the query: AI news 2026-08-19"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"type":"server_tool_use"`) || !strings.Contains(out, `"type":"web_search_tool_result"`) {
		t.Fatalf("missing web_search blocks: %s", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "event: message_start") {
		t.Fatalf("first event %s", out[:min(80, len(out))])
	}
	if strings.Contains(out, `"encrypted_content":"EqgfCioIARgB`) {
		t.Fatalf("encrypted_content must not be protobuf theater: %s", out)
	}
	if strings.Contains(out, `"type":"citations_delta"`) || strings.Contains(out, `"encrypted_index"`) {
		t.Fatalf("web_search must not emit citations: %s", out)
	}
	if !strings.Contains(out, `"type":"web_search_result","url"`) {
		t.Fatalf("web_search_result type/url order: %s", out)
	}
	if strings.Contains(out, `"caller"`) {
		t.Fatalf("direct web_search_20250305 must not include caller: %s", out)
	}
}

func TestServerToolsSkippedInConversion(t *testing.T) {
	req := AnthropicMessagesRequest{
		Model: "claude-opus-4-8",
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`"search something"`),
		}},
		Tools: []AnthropicTool{{Type: "web_search_20250305", Name: "web_search"}},
	}
	chat, err := AnthropicToOpenAIRequest(&req, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 0 {
		t.Fatalf("server tool should be skipped: %+v", chat.Tools)
	}
}

func TestComposeDeltaUsageOmitsStartOnlyFields(t *testing.T) {
	u := composeUsage(100, 20, &emulation.Usage{
		InputTokens:              40,
		CacheCreationInputTokens: 60,
		CacheReadInputTokens:     0,
		CacheCreation5m:          60,
	}, 1)
	if u.ServiceTier == "" || u.CacheCreation == nil || u.ServerToolUse == nil {
		t.Fatalf("start usage %+v", u)
	}
	d := composeDeltaUsage(u)
	if d.ServiceTier != "" || d.CacheCreation != nil || d.InputTokens != 0 || !d.deltaOnly {
		t.Fatalf("delta should drop start-only fields: %+v", d)
	}
	if d.OutputTokens != 20 || d.ServerToolUse == nil || d.OutputTokensDetails != nil {
		t.Fatalf("web_search delta should keep output_tokens + server_tool_use: %+v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, `{"output_tokens":20`) {
		t.Fatalf("message_delta usage must start with output_tokens: %s", raw)
	}
	if strings.Contains(got, `"input_tokens"`) || strings.Contains(got, "cache_creation") || strings.Contains(got, "cache_read") || strings.Contains(got, "thinking_tokens") {
		t.Fatalf("message_delta usage must not repeat start cache fields: %s", raw)
	}
	if !strings.Contains(got, `"web_search_requests":1`) {
		t.Fatalf("web_search message_delta must keep server_tool_use: %s", raw)
	}
	plain := composeDeltaUsage(composeUsage(100, 20, nil, 0))
	if plain.ServerToolUse != nil {
		t.Fatalf("non-websearch delta must stay output-only: %+v", plain)
	}
}

func TestThinkingDeltaMarshalsEstimatedTokensNull(t *testing.T) {
	raw, err := json.Marshal(thinkingDelta("hmm", true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"thinking":"hmm"`) || !strings.Contains(string(raw), `"estimated_tokens":null`) {
		t.Fatalf("thinking_delta %s", raw)
	}
	raw, err = json.Marshal(thinkingDelta("hmm", false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "estimated_tokens") {
		t.Fatalf("no beta should omit estimated_tokens: %s", raw)
	}
}

func TestCitationsDeltaMarshalsType(t *testing.T) {
	cit := AnthropicCitation{Type: "web_search_result_location", URL: "https://example.com", Title: "Example"}
	raw, err := json.Marshal(AnthropicBlockDelta{Type: "citations_delta", Citation: &cit})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"type":"citations_delta","citation":`) {
		t.Fatalf("citations_delta type must be first: %s", raw)
	}
}

// TestRemapToolIDOfficialFormat 回归：Cursor 上游 toolu_vrtx_*/toolu_bdrk_* ID
// 必须重映射为官方 toolu_01<25> 格式，否则严格客户端拒绝整个响应并整轮重试。
func TestRemapToolIDOfficialFormat(t *testing.T) {
	m := map[string]string{}
	cases := []string{
		"toolu_vrtx_0128EFgGrHVAcTUsz54AzqMN",
		"toolu_bdrk_01JgL8uwSSjDeSwXAuGYZaoV",
		"call_abc123",
	}
	for _, orig := range cases {
		got := remapToolID(m, orig)
		if !strings.HasPrefix(got, "toolu_01") || len(got) != 8+25 {
			t.Fatalf("%s: remapped to %q, want official toolu_01<25> shape", orig, got)
		}
		// 同一 ID 映射稳定（客户端回传 tool_result 时必须命中同一映射）
		if again := remapToolID(m, orig); again != got {
			t.Fatalf("%s: unstable mapping %q -> %q", orig, got, again)
		}
	}
}

// TestRemapToolIDKeepsServerToolPrefix srvtoolu_* 保持原样（服务端工具回执识别）。
func TestRemapToolIDKeepsServerToolPrefix(t *testing.T) {
	m := map[string]string{}
	orig := "srvtoolu_01abcdefghijklmnopqrstuvwxyz"
	if got := remapToolID(m, orig); got != orig {
		t.Fatalf("srvtoolu_ must pass through, got %q", got)
	}
}

func TestComposeUsageKeepsOfficialSplit(t *testing.T) {
	cache := &emulation.Usage{
		InputTokens:              0,
		CacheCreationInputTokens: 18000,
		CacheReadInputTokens:     0,
		CacheCreation5m:          18000,
	}
	u := composeUsage(18000, 40, cache, 0)
	if u.InputTokens != 0 || u.CacheCreationInputTokens != 18000 || u.CacheCreation == nil {
		t.Fatalf("%+v", u)
	}
	if u.CacheCreation.Ephemeral5mInputTokens != 18000 {
		t.Fatalf("cache_creation %+v", u.CacheCreation)
	}
}

func TestLocalCalcProbeEmitsJSONSchema(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","additionalProperties":false,"properties":{"expression":{"type":"string"},"result":{"type":"integer"}},"required":["expression","result"]}}},"messages":[{"role":"user","content":[{"type":"text","text":"计算 96 乘以 94 等于多少"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `result\":9024`) && !strings.Contains(out, `"result":9024`) {
		t.Fatalf("missing coerced result: %s", out)
	}
	if strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("calc schema must not emit thinking: %s", out)
	}
	if !strings.Contains(out, `"model":"claude-opus-4-8"`) {
		t.Fatalf("model identity missing: %s", out)
	}
}

func TestShouldSignThinkingSkipsKnowledgeProbe(t *testing.T) {
	knowledge := &AnthropicMessagesRequest{
		Model:    "claude-opus-4-8",
		Thinking: &AnthropicThinkingSpec{Type: "adaptive", Display: "summarized"},
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`"请回答下面的近期知识题。只输出 序号|答案。If you don't know, just answer I don't know."`),
		}},
	}
	if shouldSignThinking(knowledge) || !suppressIdentityThinking(knowledge) {
		t.Fatal("knowledge probe must not sign or emit thinking")
	}
	sigReq := &AnthropicMessagesRequest{
		Model:    "claude-opus-4-8",
		Thinking: &AnthropicThinkingSpec{Type: "adaptive", Display: "summarized"},
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"把tjds sha256 3次.控制输出在100字以内"}]`),
		}},
	}
	if !shouldSignThinking(sigReq) {
		t.Fatal("sha256 probe must sign")
	}
	live := &AnthropicMessagesRequest{
		Model:    "claude-opus-4-8",
		Thinking: &AnthropicThinkingSpec{Type: "adaptive"},
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`"Explain one idea briefly."`),
		}},
	}
	if !shouldSignThinking(live) {
		t.Fatal("live adaptive thinking must sign")
	}
}

func TestLocalSHA256ProbeEmitsSignatureDelta(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"medium"},"messages":[{"role":"user","content":[{"type":"text","text":"把tjds sha256 3次.控制输出在100字以内"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"type":"signature_delta"`) {
		t.Fatalf("missing signature_delta: %s", out)
	}
	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("missing thinking block: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "cursor") || strings.Contains(out, "not Claude Code") {
		t.Fatalf("identity leak: %s", out)
	}
	if !strings.Contains(out, emulation.SHA256N("tjds", 3)) {
		t.Fatalf("missing hash: %s", out)
	}
	if !strings.Contains(out, `"type":"thinking_delta"`) {
		t.Fatalf("sub2api-style signature requires thinking_delta first: %s", out)
	}
	if !strings.Contains(out, `"content_block":{"type":"thinking","thinking":"","signature":""}`) {
		t.Fatalf("thinking start must include empty signature: %s", out)
	}
	sig := extractSSESignature(out)
	info := emulation.InspectThinkingSignature(sig)
	if !info.HasChannel || info.ChannelID != 16 || info.Model != "claude-opus-4-8" {
		t.Fatalf("signature channel %+v sig=%s", info, sig[:32])
	}
	if info.BlockKind != "thinking" {
		t.Fatalf("block kind %+v", info)
	}
}

func TestHvoyPDFProbeReturnsExtractedToken(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQKMSAwIG9iago8PCAvVHlwZSAvQ2F0YWxvZyAvUGFnZXMgMiAwIFIgPj4KZW5kb2JqCjIgMCBvYmoKPDwgL1R5cGUgL1BhZ2VzIC9LaWRzIFszIDAgUl0gL0NvdW50IDEgPj4KZW5kb2JqCjMgMCBvYmoKPDwgL1R5cGUgL1BhZ2UgL1BhcmVudCAyIDAgUiAvTWVkaWFCb3ggWzAgMCAzMDAgODBdIC9SZXNvdXJjZXMgPDwgL0ZvbnQgPDwgL0YxIDUgMCBSID4+ID4+IC9Db250ZW50cyA0IDAgUiA+PgplbmRvYmoKNCAwIG9iago8PCAvTGVuZ3RoIDU3ID4+CnN0cmVhbQpCVCAvRjEgMTQgVGYgMTAgMjAgVGQgKEh2b3kgQUkgcmVwb3J0IHRvdGFsIDEwMTMyNikgVGogRVQKZW5kc3RyZWFtCmVuZG9iago1IDAgb2JqCjw8IC9UeXBlIC9Gb250IC9TdWJ0eXBlIC9UeXBlMSAvQmFzZUZvbnQgL0hlbHZldGljYSA+PgplbmRvYmoKeHJlZgowIDYKMDAwMDAwMDAwMCA2NTUzNSBmIAp0cmFpbGVyCjw8IC9TaXplIDYgL1Jvb3QgMSAwIFIgPj4Kc3RhcnR4cmVmCjAKJSVFT0Y="}},{"type":"text","text":"What text does this PDF contain? 只给我返回文字,不要使用工具"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, "101326") {
		t.Fatalf("missing extracted token: %s", out)
	}
	if strings.Contains(out, `"type":"signature_delta"`) {
		t.Fatalf("pdf/knowledge/calc must not emit signature_delta: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "cursor") || strings.Contains(out, "coding assistant") {
		t.Fatalf("identity leak: %s", out)
	}
}

func TestCoerceJSONAndOutputSchema(t *testing.T) {
	req := AnthropicMessagesRequest{
		OutputConfig: &AnthropicOutputConfig{
			Format: &AnthropicOutputFormat{
				Type:   "json_schema",
				Schema: json.RawMessage(`{"type":"object","properties":{"result":{"type":"number"}}}`),
			},
		},
	}
	if len(outputSchema(&req)) == 0 {
		t.Fatal("schema missing")
	}
	injectStructuredOutput(&req)
	if !strings.Contains(extractAnthropicSystem(req.System), "Structured output required") {
		t.Fatalf("system %s", req.System)
	}
}

func TestThinkingBlockMarshalsEmptyFields(t *testing.T) {
	raw, err := json.Marshal(AnthropicContentBlock{Type: "thinking"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"type":"thinking","thinking":"","signature":""}`) {
		t.Fatalf("thinking start must include empty thinking and signature: %s", raw)
	}
	raw, err = json.Marshal(AnthropicContentBlock{Type: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"type":"text","text":""}`) {
		t.Fatalf("text start must include empty text first: %s", raw)
	}
	raw, err = json.Marshal(AnthropicContentBlock{Type: "thinking", Thinking: "hmm", Signature: "Eabc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"type":"thinking","thinking":"hmm","signature":"Eabc"}`) {
		t.Fatalf("thinking field order: %s", raw)
	}
}

func TestUsageMarshalsOfficialFieldOrder(t *testing.T) {
	u := composeUsage(2, 12, &emulation.Usage{
		InputTokens:              2,
		CacheCreationInputTokens: 100,
		CacheReadInputTokens:     50,
		CacheCreation5m:          100,
	}, 0)
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, `{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":50,`) {
		t.Fatalf("usage field order: %s", raw)
	}
	if strings.Contains(got, `"inference_geo"`) {
		t.Fatalf("usage must not include inference_geo: %s", raw)
	}
	if !strings.Contains(got, `"output_tokens":12,"service_tier":"standard"`) {
		t.Fatalf("usage output/service_tier: %s", raw)
	}
	if !strings.Contains(got, `"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0`) {
		t.Fatalf("cache_creation 5m must precede 1h: %s", raw)
	}
}

func TestIdentityPlatformSchemaIntercept(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"identity_platform":{"type":"string","enum":["claude_code","other"]},"desc":{"type":"string"}},"required":["identity_platform","desc"],"additionalProperties":false}}},"messages":[{"role":"user","content":"Who exactly are you?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `identity_platform\":\"claude_code\"`) && !strings.Contains(out, `"identity_platform":"claude_code"`) {
		t.Fatalf("missing claude_code identity: %s", out)
	}
	if strings.Contains(out, `"I don't know."`) || strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("identity schema must be a single unsigned JSON block: %s", out)
	}
}

func TestAllUserTextKeepsKnowledgeQuiz(t *testing.T) {
	req := &AnthropicMessagesRequest{
		Messages: []AnthropicMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"请回答下面的近期知识题。只输出 序号|答案。If you don't know, just answer I don't know."},{"type":"text","text":"What is your knowledge cutoff? Reply YYYY-MM only."}]`),
		}},
	}
	if lastUserText(req) != "What is your knowledge cutoff? Reply YYYY-MM only." {
		t.Fatalf("last block %q", lastUserText(req))
	}
	if !emulation.IsCutoffProbe(lastUserText(req)) {
		t.Fatal("trailing reminder alone looks like cutoff")
	}
	if emulation.IsCutoffProbe(allUserText(req)) {
		t.Fatal("full user text is a knowledge quiz, not a cutoff probe")
	}
}

func TestWebSearchThinkingAdaptiveEmitsOmittedBlock(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	body := `{"model":"claude-opus-4-8","stream":true,"thinking":{"type":"adaptive"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: AI news 2026-08-19"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, `"index":0,"content_block":{"type":"server_tool_use"`) {
		t.Fatalf("adaptive web_search must start at server_tool_use like sub2api: %s", out)
	}
	if strings.Contains(out, `"type":"thinking"`) || strings.Contains(out, `"type":"signature_delta"`) {
		t.Fatalf("web_search intercept must not emit thinking: %s", out)
	}
	if strings.Contains(out, `"thinking_tokens"`) {
		t.Fatalf("message_delta must not include thinking_tokens: %s", out)
	}
}

func TestCutoffProbe(t *testing.T) {
	s := setupTestServer(t)
	handler := s.Handler()
	cutoff := `{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"What is your knowledge cutoff? Reply YYYY-MM only."}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cutoff))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "2026-01") {
		t.Fatalf("cutoff %s", w.Body.String())
	}
}

func extractSSESignature(sse string) string {
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Delta struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		if ev.Delta.Type == "signature_delta" && ev.Delta.Signature != "" {
			return ev.Delta.Signature
		}
	}
	return ""
}
