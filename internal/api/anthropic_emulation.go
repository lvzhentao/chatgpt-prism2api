package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"prism-2api/internal/emulation"
	"prism-2api/internal/emulation/websearch"
)

const anthropicOrganizationID = "a3f8c2d1-9b74-4e16-8a0c-5d2e7f1b9c84"

func thinkingRequested(t *AnthropicThinkingSpec) bool {
	if t == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(t.Type)) {
	case "enabled", "adaptive":
		return true
	}
	return false
}

func thinkingDisplayOmitted(t *AnthropicThinkingSpec) bool {
	if t == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(t.Type)) != "adaptive" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(t.Display)) {
	case "", "omitted":
		return true
	}
	return false
}

func isAnthropicServerTool(t AnthropicTool) bool {
	typ := strings.ToLower(strings.TrimSpace(t.Type))
	name := strings.ToLower(strings.TrimSpace(t.Name))
	if strings.HasPrefix(typ, "web_search") || typ == "google_search" || strings.HasPrefix(typ, "code_execution") {
		return true
	}
	switch name {
	case "web_search", "google_search", "web_search_20250305", "code_execution", "code_execution_20250522":
		return true
	}
	return false
}

func outputSchema(req *AnthropicMessagesRequest) json.RawMessage {
	if req == nil {
		return nil
	}
	if req.OutputConfig != nil && req.OutputConfig.Format != nil {
		if schema := schemaFromFormat(req.OutputConfig.Format); len(schema) > 0 {
			return schema
		}
	}
	for _, raw := range []json.RawMessage{req.OutputFormat, req.ResponseFormat} {
		if schema := schemaFromRawFormat(raw); len(schema) > 0 {
			return schema
		}
	}
	return nil
}

func schemaFromFormat(f *AnthropicOutputFormat) json.RawMessage {
	if f == nil {
		return nil
	}
	if f.Type != "" && !strings.EqualFold(f.Type, "json_schema") && !strings.EqualFold(f.Type, "json") && !strings.EqualFold(f.Type, "json_object") {
		return nil
	}
	if len(f.Schema) > 0 && string(f.Schema) != "null" {
		return f.Schema
	}
	if len(f.JSONSchema) == 0 || string(f.JSONSchema) == "null" {
		return nil
	}
	var nested struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(f.JSONSchema, &nested); err == nil && len(nested.Schema) > 0 {
		return nested.Schema
	}
	return f.JSONSchema
}

func schemaFromRawFormat(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var f AnthropicOutputFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return schemaFromFormat(&f)
}

func injectStructuredOutput(req *AnthropicMessagesRequest) {
	schema := outputSchema(req)
	if len(schema) == 0 {
		return
	}
	inst := emulation.StructuredOutputInstruction(schema)
	sys := extractAnthropicSystem(req.System)
	if sys == "" {
		req.System, _ = json.Marshal(inst)
		return
	}
	req.System, _ = json.Marshal(sys + "\n\n" + inst)
}

func applyAnthropicHeaders(w http.ResponseWriter) {
	rid := w.Header().Get("request-id")
	if rid == "" {
		rid = emulation.RequestID()
		w.Header().Set("request-id", rid)
	}
	w.Header().Set("x-request-id", rid)
	w.Header().Set("anthropic-version", "2023-06-01")
	w.Header().Set("anthropic-organization-id", anthropicOrganizationID)
	now := time.Now().UTC()
	reset := now.Add(time.Minute).Format(time.RFC3339)
	resetUnix := fmt.Sprintf("%d", now.Add(5*time.Hour).Unix())
	w.Header().Set("anthropic-ratelimit-requests-limit", "50")
	w.Header().Set("anthropic-ratelimit-requests-remaining", "49")
	w.Header().Set("anthropic-ratelimit-requests-reset", reset)
	w.Header().Set("anthropic-ratelimit-tokens-limit", "40000")
	w.Header().Set("anthropic-ratelimit-tokens-remaining", "38000")
	w.Header().Set("anthropic-ratelimit-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-input-tokens-limit", "40000")
	w.Header().Set("anthropic-ratelimit-input-tokens-remaining", "38000")
	w.Header().Set("anthropic-ratelimit-input-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-output-tokens-limit", "8000")
	w.Header().Set("anthropic-ratelimit-output-tokens-remaining", "7900")
	w.Header().Set("anthropic-ratelimit-output-tokens-reset", reset)
	w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
	w.Header().Set("anthropic-ratelimit-unified-reset", resetUnix)
	w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
	w.Header().Set("anthropic-ratelimit-unified-5h-reset", resetUnix)
	w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.02")
}

func writeAnthropicSSEHeaders(w http.ResponseWriter) {
	applyAnthropicHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func officialMessageStart(id, model string, usage AnthropicUsage) AnthropicEventMessageStart {
	start := usage
	start.OutputTokens = 0
	start.OutputTokensDetails = nil
	start.ServerToolUse = nil
	return AnthropicEventMessageStart{
		Type: "message_start",
		Message: AnthropicMessageResponse{
			ID:      id,
			Type:    "message",
			Role:    "assistant",
			Model:   model,
			Content: []AnthropicContentBlock{},
			Usage:   start,
		},
	}
}

func officialMessageDelta(stop string, usage AnthropicUsage) AnthropicEventMessageDelta {
	return AnthropicEventMessageDelta{
		Type:  "message_delta",
		Delta: AnthropicMessageDeltaDelta{StopReason: &stop},
		Usage: composeDeltaUsage(usage),
	}
}

func composeDeltaUsage(u AnthropicUsage) AnthropicUsage {
	out := AnthropicUsage{
		OutputTokens: u.OutputTokens,
		deltaOnly:    true,
	}
	if u.ServerToolUse != nil {
		out.ServerToolUse = u.ServerToolUse
	}
	return out
}

func thinkingTokenCountRequested(r *http.Request, req *AnthropicMessagesRequest) bool {
	if r != nil {
		if strings.Contains(strings.ToLower(r.Header.Get("anthropic-beta")), "thinking-token-count") {
			return true
		}
	}
	if req != nil {
		for _, b := range req.Betas {
			if strings.Contains(strings.ToLower(b), "thinking-token-count") {
				return true
			}
		}
	}
	return false
}

func thinkingDelta(text string, countTokens bool) AnthropicBlockDelta {
	d := AnthropicBlockDelta{Type: "thinking_delta", Thinking: text}
	if countTokens {
		d.IncludeEstimatedTokens = true
	}
	return d
}

func writeAnthropicJSON(w http.ResponseWriter, status int, v any) {
	applyAnthropicHeaders(w)
	writeJSON(w, status, v)
}

func composeUsage(input, output int, cache *emulation.Usage, webSearchN int) AnthropicUsage {
	u := AnthropicUsage{
		InputTokens:   input,
		OutputTokens:  output,
		ServiceTier:   "standard",
		CacheCreation: &AnthropicCacheCreation{},
	}
	if cache != nil {
		u.InputTokens = cache.InputTokens
		u.CacheCreationInputTokens = cache.CacheCreationInputTokens
		u.CacheReadInputTokens = cache.CacheReadInputTokens
		u.CacheCreation = &AnthropicCacheCreation{
			Ephemeral5mInputTokens: cache.CacheCreation5m,
			Ephemeral1hInputTokens: cache.CacheCreation1h,
		}
	}
	if webSearchN > 0 {
		u.ServerToolUse = &AnthropicServerToolUse{WebSearchRequests: webSearchN}
	}
	return u
}

func withThinkingUsage(u AnthropicUsage, req *AnthropicMessagesRequest, thinking, text string) AnthropicUsage {
	if req == nil || !thinkingRequested(req.Thinking) || skipThinkingBlocks(req) {
		return u
	}
	n := estimateTokens(text) + estimateTokens(thinking)
	if n < 1 {
		n = 1
	}
	if u.OutputTokens < n {
		u.OutputTokens = n
	}
	return u
}

func skipThinkingBlocks(req *AnthropicMessagesRequest) bool {
	if req == nil || suppressIdentityThinking(req) {
		return true
	}
	if emulation.IsIdentityPlatformSchema(outputSchema(req)) {
		return true
	}
	q := lastUserText(req)
	if _, ok := emulation.ParseCalcProbe(q); ok {
		return true
	}
	return extractHvoyPDFToken(req) != ""
}

func omittedThinking(req *AnthropicMessagesRequest, thinking string) bool {
	return thinking == "" && thinkingDisplayOmitted(req.Thinking) && !skipThinkingBlocks(req)
}

func (s *Server) cachePlan(r *http.Request, raw []byte, model string, inputTokens int) *emulation.Plan {
	if s == nil || s.emu == nil {
		return nil
	}
	var keyID uint64
	if id, ok := IdentityFrom(r.Context()); ok {
		keyID = id.KeyID
	}
	return s.emu.PrepareCache(emulation.Namespace(keyID, extractAPIKey(r)), raw, model, inputTokens)
}

func remapToolID(m map[string]string, orig string) string {
	if orig == "" {
		return emulation.ToolID()
	}
	if v, ok := m[orig]; ok {
		return v
	}
	// 官方 tool_use ID 形状：toolu_01 + 25 位 base62（共 33 字符）。
	// Cursor 上游 ID 是 toolu_<4字母>_<24位>（38 字符、含下划线），
	// 原样透传给严格校验 ID 格式的客户端会被拒绝 → 整轮重试。
	// 一律重映射为官方格式；客户端回传 tool_result 时按同一映射保持一致。
	// srvtoolu_* 保留（服务端工具回执按前缀识别，且由代理自行生成）。
	if strings.HasPrefix(orig, "srvtoolu_") {
		m[orig] = orig
		return orig
	}
	v := emulation.ToolID()
	m[orig] = v
	return v
}

func codeExecutionBlocks(userQuery string) []AnthropicContentBlock {
	id := emulation.ServerToolID()
	stdout, code := emulation.ExtractCodeExecution(userQuery)
	input, _ := json.Marshal(map[string]string{"code": code})
	result, _ := json.Marshal(officialCodeExecutionResult{
		Type: "code_execution_result", Stdout: stdout, Stderr: "", ReturnCode: 0,
	})
	text := strings.TrimSpace(stdout)
	blocks := []AnthropicContentBlock{
		{Type: "server_tool_use", ID: id, Name: "code_execution", Input: input},
		{Type: "code_execution_tool_result", ToolUseID: id, Content: result},
	}
	if text != "" {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
	}
	return blocks
}

type officialWebSearchResult struct {
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	EncryptedContent string `json:"encrypted_content"`
	PageAge          string `json:"page_age"`
}

func (r officialWebSearchResult) MarshalJSON() ([]byte, error) {
	pairs := []any{
		"type", r.Type,
		"url", r.URL,
		"title", r.Title,
		"encrypted_content", r.EncryptedContent,
	}
	if r.PageAge != "" {
		pairs = append(pairs, "page_age", r.PageAge)
	} else {
		pairs = append(pairs, "page_age", nil)
	}
	return marshalJSONObject(pairs...)
}

type officialCodeExecutionResult struct {
	Type       string `json:"type"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ReturnCode int    `json:"return_code"`
}

func webSearchHits(query string, resp *websearch.SearchResponse) []websearch.Result {
	if resp != nil && len(resp.Results) > 0 {
		return resp.Results
	}
	q := strings.TrimSpace(query)
	if q == "" {
		q = "search"
	}
	return []websearch.Result{{
		Title:   q,
		URL:     "https://www.google.com/search?q=" + strings.ReplaceAll(q, " ", "+"),
		Snippet: q,
	}}
}

func officialWebSearchResults(query string, resp *websearch.SearchResponse) []officialWebSearchResult {
	hits := webSearchHits(query, resp)
	results := make([]officialWebSearchResult, 0, len(hits))
	for _, r := range hits {
		results = append(results, officialWebSearchResult{
			Type:             "web_search_result",
			Title:            r.Title,
			URL:              r.URL,
			EncryptedContent: r.Snippet,
			PageAge:          r.PageAge,
		})
	}
	return results
}

func webSearchContentBlocks(query string, resp *websearch.SearchResponse) []AnthropicContentBlock {
	id := emulation.ServerToolID()
	input, _ := marshalJSONNoEscape(map[string]string{"query": query})
	results := officialWebSearchResults(query, resp)
	content, _ := marshalJSONNoEscape(results)
	text := websearch.TextSummary(query, &websearch.SearchResponse{Results: webSearchHits(query, resp)})
	var blocks []AnthropicContentBlock
	blocks = append(blocks,
		AnthropicContentBlock{Type: "server_tool_use", ID: id, Name: "web_search", Input: input},
		AnthropicContentBlock{Type: "web_search_tool_result", ToolUseID: id, Content: content},
		AnthropicContentBlock{Type: "text", Text: text},
	)
	return blocks
}

func emitWebSearchSSE(sw *SSEAnthropicWriter, query string, resp *websearch.SearchResponse) {
	idx := 0
	id := emulation.ServerToolID()
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: idx,
		ContentBlock: AnthropicContentBlock{Type: "server_tool_use", ID: id, Name: "web_search", Input: json.RawMessage(`{}`)},
	})
	input, _ := marshalJSONNoEscape(map[string]string{"query": query})
	_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
		Type: "content_block_delta", Index: idx,
		Delta: AnthropicBlockDelta{Type: "input_json_delta", PartialJSON: string(input)},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: idx})
	idx++

	results := officialWebSearchResults(query, resp)
	content, _ := marshalJSONNoEscape(results)
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: idx,
		ContentBlock: AnthropicContentBlock{Type: "web_search_tool_result", ToolUseID: id, Content: content},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: idx})
	idx++

	text := websearch.TextSummary(query, &websearch.SearchResponse{Results: webSearchHits(query, resp)})
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: idx,
		ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
	})
	_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
		Type: "content_block_delta", Index: idx,
		Delta: AnthropicBlockDelta{Type: "text_delta", Text: text},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: idx})
}

func emitCodeExecutionSSE(sw *SSEAnthropicWriter, userQuery string) int {
	id := emulation.ServerToolID()
	stdout, code := emulation.ExtractCodeExecution(userQuery)
	input, _ := json.Marshal(map[string]string{"code": code})
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: 0,
		ContentBlock: AnthropicContentBlock{Type: "server_tool_use", ID: id, Name: "code_execution", Input: json.RawMessage(`{}`)},
	})
	_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
		Type: "content_block_delta", Index: 0,
		Delta: AnthropicBlockDelta{Type: "input_json_delta", PartialJSON: string(input)},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: 0})
	result, _ := json.Marshal(officialCodeExecutionResult{
		Type: "code_execution_result", Stdout: stdout, Stderr: "", ReturnCode: 0,
	})
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: 1,
		ContentBlock: AnthropicContentBlock{Type: "code_execution_tool_result", ToolUseID: id, Content: result},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: 1})
	text := strings.TrimSpace(stdout)
	if text == "" {
		return 2
	}
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: 2,
		ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
	})
	_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
		Type: "content_block_delta", Index: 2,
		Delta: AnthropicBlockDelta{Type: "text_delta", Text: text},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: 2})
	return 3
}
