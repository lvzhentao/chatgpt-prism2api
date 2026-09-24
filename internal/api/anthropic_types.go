package api

import (
	"bytes"
	"encoding/json"
)

// Anthropic Messages API 请求与响应类型定义
// 遵循 https://docs.anthropic.com/en/api/messages 规范。

// AnthropicMessagesRequest 是 /v1/messages 的请求体。
type AnthropicMessagesRequest struct {
	Model             string                 `json:"model"`
	Messages          []AnthropicMessage     `json:"messages"`
	System            json.RawMessage        `json:"system,omitempty"` // string 或 []AnthropicContentBlock
	MaxTokens         int                    `json:"max_tokens,omitempty"`
	Stream            bool                   `json:"stream,omitempty"`
	Temperature       *float64               `json:"temperature,omitempty"`
	TopP              *float64               `json:"top_p,omitempty"`
	TopK              *int                   `json:"top_k,omitempty"`
	Tools             []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice        json.RawMessage        `json:"tool_choice,omitempty"`
	Thinking          *AnthropicThinkingSpec `json:"thinking,omitempty"`
	StopSequences     []string               `json:"stop_sequences,omitempty"`
	Betas             []string               `json:"betas,omitempty"`
	OutputConfig      *AnthropicOutputConfig `json:"output_config,omitempty"`
	OutputFormat      json.RawMessage        `json:"output_format,omitempty"`
	ResponseFormat    json.RawMessage        `json:"response_format,omitempty"`
	Metadata          map[string]any         `json:"metadata,omitempty"`
	ContextManagement map[string]any         `json:"context_management,omitempty"`
}

// AnthropicOutputConfig 是输出配置（如 effort 控制）。
type AnthropicOutputConfig struct {
	Effort string                 `json:"effort,omitempty"` // "low" | "medium" | "high"
	Format *AnthropicOutputFormat `json:"format,omitempty"`
}

// AnthropicOutputFormat 是结构化输出声明（json_schema）。
type AnthropicOutputFormat struct {
	Type       string          `json:"type,omitempty"`
	Schema     json.RawMessage `json:"schema,omitempty"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// AnthropicCacheControl 是提示词缓存控制说明。
type AnthropicCacheControl struct {
	Type  string `json:"type"` // "ephemeral"
	TTL   string `json:"ttl,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// AnthropicThinkingSpec 是思考配置。
type AnthropicThinkingSpec struct {
	Type         string `json:"type"` // "enabled" | "adaptive" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

// AnthropicMessage 是单条消息。
// Claude Code 2026 延迟工具模式把工具定义作为无 role 消息下发：
// {"name":"...","description":"...","input_schema":{...}}（无 content）。
// 这些字段必须保留，供 AnthropicToOpenAIRequest 提取为工具声明。
type AnthropicMessage struct {
	Role         string          `json:"role"` // "user" | "assistant"
	Content      json.RawMessage `json:"content"`
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
}

// AnthropicContentBlock 是通用内容块。
type AnthropicContentBlock struct {
	Type         string                 `json:"type"` // "text" | "image" | "document" | "tool_use" | "tool_result" | "thinking" | "server_tool_use"
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
	// text
	Text string `json:"text,omitempty"`

	// image / document
	Source *AnthropicImageSource `json:"source,omitempty"`
	Title  string                `json:"title,omitempty"`

	// tool_use / server_tool_use
	ID     string           `json:"id,omitempty"`
	Name   string           `json:"name,omitempty"`
	Input  json.RawMessage  `json:"input,omitempty"`
	Caller *AnthropicCaller `json:"caller,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string 或 []AnthropicContentBlock
	IsError   bool            `json:"is_error,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// text citations (web_search)
	Citations []AnthropicCitation `json:"citations,omitempty"`
}

// AnthropicCitation 是官方 text 块上的 web_search 引用。
type AnthropicCitation struct {
	Type           string `json:"type"`
	URL            string `json:"url,omitempty"`
	Title          string `json:"title,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
}

func (c AnthropicCitation) MarshalJSON() ([]byte, error) {
	pairs := []any{"type", c.Type}
	if c.URL != "" {
		pairs = append(pairs, "url", c.URL)
	}
	if c.Title != "" {
		pairs = append(pairs, "title", c.Title)
	}
	if c.EncryptedIndex != "" {
		pairs = append(pairs, "encrypted_index", c.EncryptedIndex)
	}
	if c.CitedText != "" {
		pairs = append(pairs, "cited_text", c.CitedText)
	}
	return marshalJSONObject(pairs...)
}

// MarshalJSON 按官方字段顺序输出，避免 encoding/json 把 type 排到字母序末尾。
func (b AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "thinking":
		return marshalJSONObject("type", "thinking", "thinking", b.Thinking, "signature", b.Signature)
	case "text":
		pairs := []any{"type", "text", "text", b.Text}
		if len(b.Citations) > 0 {
			pairs = append(pairs, "citations", b.Citations)
		}
		return marshalJSONObject(pairs...)
	case "tool_use", "server_tool_use":
		pairs := []any{"type", b.Type}
		if b.ID != "" {
			pairs = append(pairs, "id", b.ID)
		}
		if b.Name != "" {
			pairs = append(pairs, "name", b.Name)
		}
		if b.Caller != nil {
			pairs = append(pairs, "caller", b.Caller)
		}
		input := b.Input
		if len(input) == 0 || string(input) == "null" {
			input = json.RawMessage(`{}`)
		}
		pairs = append(pairs, "input", json.RawMessage(input))
		return marshalJSONObject(pairs...)
	case "tool_result", "web_search_tool_result", "code_execution_tool_result",
		"bash_code_execution_tool_result", "text_editor_code_execution_tool_result",
		"mcp_tool_result":
		pairs := []any{"type", b.Type}
		if b.ToolUseID != "" {
			pairs = append(pairs, "tool_use_id", b.ToolUseID)
		}
		if b.Caller != nil {
			pairs = append(pairs, "caller", b.Caller)
		}
		if len(b.Content) > 0 {
			pairs = append(pairs, "content", json.RawMessage(b.Content))
		}
		if b.IsError {
			pairs = append(pairs, "is_error", true)
		}
		return marshalJSONObject(pairs...)
	case "image", "document":
		pairs := []any{"type", b.Type}
		if b.Title != "" {
			pairs = append(pairs, "title", b.Title)
		}
		if b.Source != nil {
			pairs = append(pairs, "source", b.Source)
		}
		return marshalJSONObject(pairs...)
	default:
		pairs := []any{"type", b.Type}
		if b.Text != "" {
			pairs = append(pairs, "text", b.Text)
		}
		if b.Thinking != "" {
			pairs = append(pairs, "thinking", b.Thinking)
		}
		if b.Signature != "" {
			pairs = append(pairs, "signature", b.Signature)
		}
		if b.ID != "" {
			pairs = append(pairs, "id", b.ID)
		}
		if b.Name != "" {
			pairs = append(pairs, "name", b.Name)
		}
		if b.ToolUseID != "" {
			pairs = append(pairs, "tool_use_id", b.ToolUseID)
		}
		if len(b.Input) > 0 {
			pairs = append(pairs, "input", rawJSONValue(b.Input, json.RawMessage(b.Input)))
		}
		if len(b.Content) > 0 {
			pairs = append(pairs, "content", rawJSONValue(b.Content, json.RawMessage(b.Content)))
		}
		if b.Source != nil {
			pairs = append(pairs, "source", b.Source)
		}
		if b.Title != "" {
			pairs = append(pairs, "title", b.Title)
		}
		return marshalJSONObject(pairs...)
	}
}

func rawJSONValue(raw json.RawMessage, fallback any) any {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fallback
	}
	return v
}

// AnthropicImageSource 是图片源。
type AnthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url" | "text" | "file"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// AnthropicTool 是工具定义。
type AnthropicTool struct {
	Type         string                 `json:"type,omitempty"` // web_search_20250305 / code_execution_20250522
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"`
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

// AnthropicCaller 是 2026 服务端工具调用方。官方响应里 type=direct。
type AnthropicCaller struct {
	Type   string `json:"type"`
	ToolID string `json:"tool_id,omitempty"`
}

func (c AnthropicCaller) MarshalJSON() ([]byte, error) {
	pairs := []any{"type", c.Type}
	if c.ToolID != "" {
		pairs = append(pairs, "tool_id", c.ToolID)
	}
	return marshalJSONObject(pairs...)
}

func directCaller() *AnthropicCaller {
	return &AnthropicCaller{Type: "direct"}
}

// AnthropicUsage 是用量统计。缓存字段始终输出（含 0），避免检测站把缺省当成 input=0。
type AnthropicUsage struct {
	InputTokens              int                           `json:"input_tokens"`
	OutputTokens             int                           `json:"output_tokens"`
	CacheCreationInputTokens int                           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                           `json:"cache_read_input_tokens"`
	CacheCreation            *AnthropicCacheCreation       `json:"cache_creation,omitempty"`
	ServiceTier              string                        `json:"service_tier,omitempty"`
	ServerToolUse            *AnthropicServerToolUse       `json:"server_tool_use,omitempty"`
	OutputTokensDetails      *AnthropicOutputTokensDetails `json:"output_tokens_details,omitempty"`
	deltaOnly                bool                          `json:"-"`
}

// AnthropicOutputTokensDetails 是 thinking 用量拆分。官方字段名是 thinking_tokens。
type AnthropicOutputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// AnthropicCacheCreation 是按 TTL 拆分的 cache write。
type AnthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

func (c AnthropicCacheCreation) MarshalJSON() ([]byte, error) {
	return marshalJSONObject(
		"ephemeral_5m_input_tokens", c.Ephemeral5mInputTokens,
		"ephemeral_1h_input_tokens", c.Ephemeral1hInputTokens,
	)
}

// AnthropicServerToolUse 是服务端工具用量。
type AnthropicServerToolUse struct {
	WebFetchRequests  int `json:"web_fetch_requests"`
	WebSearchRequests int `json:"web_search_requests"`
}

func (s AnthropicServerToolUse) MarshalJSON() ([]byte, error) {
	pairs := []any{"web_search_requests", s.WebSearchRequests}
	if s.WebFetchRequests != 0 {
		pairs = []any{"web_fetch_requests", s.WebFetchRequests, "web_search_requests", s.WebSearchRequests}
	}
	return marshalJSONObject(pairs...)
}

// AnthropicMessageResponse 是非流式响应。
type AnthropicMessageResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // "message"
	Role         string                  `json:"role"` // "assistant"
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"` // "end_turn" | "max_tokens" | "stop_sequence" | "tool_use"
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
	Container    *AnthropicContainer     `json:"container,omitempty"`
}

func (m AnthropicMessageResponse) MarshalJSON() ([]byte, error) {
	content := m.Content
	if content == nil {
		content = []AnthropicContentBlock{}
	}
	pairs := []any{
		"id", m.ID,
		"type", m.Type,
		"role", m.Role,
		"model", m.Model,
		"content", content,
		"stop_reason", m.StopReason,
		"stop_sequence", m.StopSequence,
		"usage", m.Usage,
	}
	if m.Container != nil {
		pairs = append(pairs, "container", m.Container)
	}
	return marshalJSONObject(pairs...)
}

// AnthropicContainer 是 code_execution 容器。
type AnthropicContainer struct {
	ID string `json:"id"`
}

// AnthropicCountTokensRequest 是 token 预估请求体（与 Messages 相同结构）。
type AnthropicCountTokensRequest struct {
	Model    string             `json:"model"`
	Messages []AnthropicMessage `json:"messages"`
	System   json.RawMessage    `json:"system,omitempty"`
	Tools    []AnthropicTool    `json:"tools,omitempty"`
}

// AnthropicCountTokensResponse 是 token 预估响应。
type AnthropicCountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// Anthropic SSE 事件模型定义

// AnthropicEventMessageStart 是 message_start 事件。
type AnthropicEventMessageStart struct {
	Type    string                   `json:"type"` // "message_start"
	Message AnthropicMessageResponse `json:"message"`
}

// AnthropicEventContentBlockStart 是 content_block_start 事件。
type AnthropicEventContentBlockStart struct {
	Type         string                `json:"type"` // "content_block_start"
	Index        int                   `json:"index"`
	ContentBlock AnthropicContentBlock `json:"content_block"`
}

// AnthropicEventContentBlockDelta 是 content_block_delta 事件。
type AnthropicEventContentBlockDelta struct {
	Type  string              `json:"type"` // "content_block_delta"
	Index int                 `json:"index"`
	Delta AnthropicBlockDelta `json:"delta"`
}

// AnthropicBlockDelta 是具体的 delta 内容。
type AnthropicBlockDelta struct {
	Type string `json:"type"` // "text_delta" | "thinking_delta" | "input_json_delta" | "signature_delta"

	// text_delta
	Text string `json:"text,omitempty"`

	// thinking_delta
	Thinking string `json:"thinking,omitempty"`

	// signature_delta
	Signature string `json:"signature,omitempty"`

	// input_json_delta
	PartialJSON string `json:"partial_json,omitempty"`

	// citations_delta
	Citation *AnthropicCitation `json:"citation,omitempty"`

	// thinking-token-count-2026-05-13：display 非 omitted 时官方仍带 null。
	IncludeEstimatedTokens bool `json:"-"`
	EstimatedTokens        *int `json:"-"`
}

func (d AnthropicBlockDelta) MarshalJSON() ([]byte, error) {
	switch d.Type {
	case "text_delta":
		return marshalJSONObject("type", "text_delta", "text", d.Text)
	case "thinking_delta":
		pairs := []any{"type", "thinking_delta", "thinking", d.Thinking}
		if d.IncludeEstimatedTokens {
			pairs = append(pairs, "estimated_tokens", d.EstimatedTokens)
		}
		return marshalJSONObject(pairs...)
	case "signature_delta":
		return marshalJSONObject("type", "signature_delta", "signature", d.Signature)
	case "input_json_delta":
		return marshalJSONObject("type", "input_json_delta", "partial_json", d.PartialJSON)
	case "citations_delta":
		pairs := []any{"type", "citations_delta"}
		if d.Citation != nil {
			pairs = append(pairs, "citation", d.Citation)
		}
		return marshalJSONObject(pairs...)
	default:
		pairs := []any{"type", d.Type}
		if d.Text != "" {
			pairs = append(pairs, "text", d.Text)
		}
		if d.Thinking != "" {
			pairs = append(pairs, "thinking", d.Thinking)
		}
		if d.Signature != "" {
			pairs = append(pairs, "signature", d.Signature)
		}
		if d.PartialJSON != "" {
			pairs = append(pairs, "partial_json", d.PartialJSON)
		}
		return marshalJSONObject(pairs...)
	}
}

func (u AnthropicUsage) MarshalJSON() ([]byte, error) {
	if u.deltaOnly {
		pairs := []any{"output_tokens", u.OutputTokens}
		if u.OutputTokensDetails != nil {
			pairs = append(pairs, "output_tokens_details", u.OutputTokensDetails)
		}
		if u.ServerToolUse != nil {
			pairs = append(pairs, "server_tool_use", u.ServerToolUse)
		}
		return marshalJSONObject(pairs...)
	}
	pairs := []any{
		"input_tokens", u.InputTokens,
		"cache_creation_input_tokens", u.CacheCreationInputTokens,
		"cache_read_input_tokens", u.CacheReadInputTokens,
	}
	if u.CacheCreation != nil {
		pairs = append(pairs, "cache_creation", u.CacheCreation)
	}
	pairs = append(pairs, "output_tokens", u.OutputTokens)
	if u.ServiceTier != "" {
		pairs = append(pairs, "service_tier", u.ServiceTier)
	}
	if u.ServerToolUse != nil {
		pairs = append(pairs, "server_tool_use", u.ServerToolUse)
	}
	if u.OutputTokensDetails != nil {
		pairs = append(pairs, "output_tokens_details", u.OutputTokensDetails)
	}
	return marshalJSONObject(pairs...)
}

func marshalJSONObject(pairs ...any) ([]byte, error) {
	if len(pairs)%2 != 0 {
		return json.Marshal(map[string]any{})
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalJSONNoEscape(pairs[i])
		if err != nil {
			return nil, err
		}
		val, err := marshalJSONNoEscape(pairs[i+1])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// AnthropicEventContentBlockStop 是 content_block_stop 事件。
type AnthropicEventContentBlockStop struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

// AnthropicEventMessageDelta 是 message_delta 事件。
type AnthropicEventMessageDelta struct {
	Type  string                     `json:"type"` // "message_delta"
	Delta AnthropicMessageDeltaDelta `json:"delta"`
	Usage AnthropicUsage             `json:"usage"`
}

// AnthropicMessageDeltaDelta 是 message_delta 中的增量状态。
type AnthropicMessageDeltaDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// AnthropicEventMessageStop 是 message_stop 事件。
type AnthropicEventMessageStop struct {
	Type string `json:"type"` // "message_stop"
}

// AnthropicEventPing 是 ping 事件。
type AnthropicEventPing struct {
	Type string `json:"type"` // "ping"
}

// AnthropicEventError 是错误事件。
type AnthropicEventError struct {
	Type  string            `json:"type"` // "error"
	Error AnthropicAPIError `json:"error"`
}

// AnthropicAPIError 是错误体。
type AnthropicAPIError struct {
	Type    string `json:"type"` // "invalid_request_error" | "authentication_error" | "api_error"
	Message string `json:"message"`
}
