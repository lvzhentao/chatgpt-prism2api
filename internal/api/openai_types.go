// Package api 实现 OpenAI 兼容的 HTTP API 层。
package api

import (
	"encoding/json"

	"prism-2api/internal/adapter"
)

// ChatCompletionRequest 是 OpenAI 兼容的 chat/completions 请求。
type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	User                string          `json:"user,omitempty"`
	Metadata            map[string]any  `json:"metadata,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
	Conversation        json.RawMessage `json:"conversation,omitempty"`
	ConversationID      string          `json:"conversation_id,omitempty"`
}

// ChatMessage 是对话消息（content 可为 string 或 []ContentPart）。
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"` // 兼容 DeepSeek 风格
}

// ContentPart 是多模态内容片段：文本 / 图片 / 文件（各协议形状见 chat_files.go）。
type ContentPart struct {
	Type     string        `json:"type"` // text | input_text | image_url | file | input_file
	Text     string        `json:"text,omitempty"`
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	File     *FilePart     `json:"file,omitempty"` // Chat Completions: {"type":"file","file":{…}}
	// Responses 把文件名与载荷平铺在块上：{"type":"input_file","filename":…,"file_data":…}
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// ImageURLPart 图片地址（支持 data: URL 与 http(s) 地址）。
type ImageURLPart struct {
	URL string `json:"url"`
}

// FilePart 是 OpenAI Chat Completions 的文件载荷。
// file_data 可以是 data: URL、裸 base64 或 http(s) 地址。
type FilePart struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// Tool 是 OpenAI function 工具定义。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具函数定义。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall 是模型输出的工具调用。
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 工具调用的函数部分。
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ModelObject 是 /v1/models 的条目。
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ChatCompletionChunk 是流式响应片段（OpenAI SSE 格式）。
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChoiceChunk `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
	// Error 是「这一轮失败了」的可辨识标记。内核在流中失败时仍会把错误文案放进
	// content（旧行为，保持对普通 chat 客户端的兼容），但额外挂上这个对象，
	// 让 /v1/responses 能区分「模型说了这句话」与「这一轮失败」，从而发
	// response.failed 而不是把错误当成助手正文 delivered。
	Error *ChunkError `json:"error,omitempty"`
}

// ChunkError 流式错误帧的载荷。
type ChunkError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ChoiceChunk 流式选择片段。
type ChoiceChunk struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

// Delta 流式增量。
type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta 流式工具调用增量。
type ToolCallDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// Usage 用量统计。
// 上游返回 context_window_status 时，PromptTokens 取真实上下文占用
// （见 ContextWindow 字段）；否则为本地近似估算。
type Usage struct {
	PromptTokens        int                          `json:"prompt_tokens"`
	CompletionTokens    int                          `json:"completion_tokens"`
	TotalTokens         int                          `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails         `json:"prompt_tokens_details,omitempty"`
	ContextWindow       *adapter.ContextWindowStatus `json:"context_window_status,omitempty"`
}

// PromptTokensDetails 是 OpenAI 官方的 prompt 细分；CachedTokens 对应模拟缓存的
// 读命中量（emulation.CacheReadInputTokens）。官方语义：cached ⊆ prompt_tokens，
// prompt_tokens 总额不变。
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}
