package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// ---------- OpenAI Responses API（/v1/responses） ----------
//
// 请求（官方格式）：
//
//	{ model, instructions?, input, tools?, stream?, max_output_tokens?,
//	  reasoning: {effort}?, text: {format}? }
//
// 非流式响应：Response 对象（output 数组 + output_text 便捷字段）。
// 流式响应：SSE 事件（response.created → output_item.added → content_part.added
// → output_text.delta → ... → response.completed）。

// ResponsesRequest 是 /v1/responses 请求体。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Tools           []responsesTool `json:"tools,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	PromptCacheKey  string          `json:"prompt_cache_key,omitempty"`
	Metadata        map[string]any  `json:"metadata,omitempty"`
	Conversation    json.RawMessage `json:"conversation,omitempty"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

// responsesTool 是 Responses API 工具定义（顶层 name/description/parameters，
// 与 Chat Completions 的 function 嵌套格式不同）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function,omitempty"` // 兼容 Chat Completions 风格
}

// responsesInputItem 是 input 数组中的条目。
// 注意 output 必须是 any：function_call_output 的 output 官方允许字符串或
// content-part 数组（如 [{"type":"input_text","text":"..."}]），string 类型会在
// 这里直接 400。tool 结果回灌时用数组形态是 SDK（如 Codex）常见写法。
type responsesInputItem struct {
	Role       string `json:"role"`
	Type       string `json:"type"` // message | function_call | function_call_output | custom_tool_call | custom_tool_call_output | reasoning
	Content    any    `json:"content,omitempty"`
	Name       string `json:"name,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	Output     any    `json:"output,omitempty"`
	Arguments  any    `json:"arguments,omitempty"`
	Input      any    `json:"input,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// responsesToChatRequest 将 Responses 请求转为内部 ChatCompletionRequest。
func responsesToChatRequest(req *ResponsesRequest) (*ChatCompletionRequest, error) {
	out := &ChatCompletionRequest{
		Model:          req.Model,
		Stream:         req.Stream,
		MaxTokens:      nil,
		Metadata:       req.Metadata,
		PromptCacheKey: req.PromptCacheKey,
		Conversation:   req.Conversation,
		ConversationID: req.ConversationID,
	}
	if req.MaxOutputTokens > 0 {
		mt := capMaxTokens(req.MaxOutputTokens)
		out.MaxTokens = &mt
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.ReasoningEffort = capEffort(req.Reasoning.Effort)
	}
	for _, t := range req.Tools {
		name, desc, params := t.Name, t.Description, t.Parameters
		if name == "" && t.Function != nil {
			name, desc, params = t.Function.Name, t.Function.Description, t.Function.Parameters
		}
		if name == "" {
			continue
		}
		// Codex 把 apply_patch 这类工具发成 freeform 的 type:"custom"（grammar，不是 JSON schema）。
		// 上游根本没有 tools 通道（我们用提示词仿真），所以按「单 input 字符串参数」描述给模型；
		// 回给客户端时再还原成 custom_tool_call/{input} 项，Codex 才认（见 customToolNames）。
		if strings.EqualFold(t.Type, "custom") {
			params = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"freeform tool input"}},"required":["input"]}`)
		} else if len(params) > 0 {
			params = normalizeJSONSchema(params)
		} else {
			params = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, Tool{
			Type:     "function",
			Function: ToolFunction{Name: name, Description: desc, Parameters: params},
		})
	}

	var msgs []ChatMessage
	if req.Instructions != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: json.RawMessage(mustJSON(req.Instructions))})
	}
	input := req.Input
	if len(input) > 0 && input[0] == '"' {
		// input 可以是字符串（单条 user 消息）
		var s string
		if err := json.Unmarshal(input, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		msgs = append(msgs, ChatMessage{Role: "user", Content: json.RawMessage(mustJSON(s))})
		out.Messages = msgs
		return out, nil
	}

	// input 三种合法形态：字符串 / 单个条目对象 / 条目数组。
	// 单对象不包数组是 SDK 常见写法（此前直接 400：
	// "cannot unmarshal object into Go value of type []responsesInputItem"）。
	input = bytes.TrimSpace(input)
	if len(input) > 0 && input[0] == '{' {
		input = append([]byte{'['}, append(input, ']')...)
	}
	var items []responsesInputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	for _, item := range items {
		switch item.Type {
		case "function_call_output", "custom_tool_call_output":
			// Codex 回程的 custom_tool_call_output 与 function_call_output 同义：
			// 都是 tool 角色消息 + call_id 对应，不合并就会落进 default 被丢弃，
			// 第二轮续接时模型看不到工具结果，上下文断裂。
			msgs = append(msgs, ChatMessage{
				Role: "tool", ToolCallID: item.CallID,
				Content: json.RawMessage(mustJSON(flattenToolOutput(item.Output))),
			})
			continue
		case "custom_tool_call":
			// 出站时 custom 工具按 {"input": <freeform文本>} 描述（见上 tools 循环），
			// 回程必须反向包回同一个形状，否则回灌后上游拿到的 arguments 对不上。
			msgs = append(msgs, ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID: item.CallID, Type: "function",
					Function: ToolCallFunction{Name: item.Name, Arguments: mustJSON(map[string]any{"input": normalizeCustomInput(item.Input)})},
				}},
			})
			continue
		case "function_call":
			msgs = append(msgs, ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID: item.CallID, Type: "function",
					Function: ToolCallFunction{Name: item.Name, Arguments: normalizeToolArguments(item.Arguments)},
				}},
			})
			continue
		}
		// reasoning 等无 role 的条目直接跳过：加密推理内容只有上游能解，转发无意义。
		// developer 是 Responses 里 system 的新名字，映射过去以免丢失顶层指令。
		role := item.Role
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "user", "system", "assistant":
			content := item.Content
			if s, ok := content.(string); ok {
				msgs = append(msgs, ChatMessage{Role: role, Content: json.RawMessage(mustJSON(s))})
			} else if content != nil {
				// content 数组（input_text/input_image…）摊平成 chat 的 content 块：
				// 文本块保留（input_text→text），图片块转 data URL（extractImages 只认 image_url）。
				if flat, ok := flattenResponsesContent(content); ok {
					msgs = append(msgs, ChatMessage{Role: role, Content: json.RawMessage(mustJSON(flat))})
				} else if b, err := json.Marshal(content); err != nil {
					return nil, fmt.Errorf("invalid content: %w", err)
				} else {
					msgs = append(msgs, ChatMessage{Role: role, Content: b})
				}
			}
		}
	}

	out.Messages = msgs
	return out, nil
}

// flattenToolOutput 把 function_call_output 的 output 摊平成纯文本。
// 官方允许 string 或 content-part 数组；数组时抽各 part 的 text 拼起来
// （input_text/output_text/refusal 块），未知形状退回 JSON 串，避免丢信息。
func flattenToolOutput(output any) string {
	if output == nil {
		return ""
	}
	if s, ok := output.(string); ok {
		return s
	}
	arr, ok := output.([]any)
	if !ok {
		if b, err := json.Marshal(output); err == nil {
			return string(b)
		}
		return ""
	}
	var sb strings.Builder
	for _, p := range arr {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		switch t {
		case "input_text", "output_text", "text", "refusal":
			if s, _ := m["text"].(string); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		default:
			if s, _ := m["text"].(string); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// normalizeToolArguments 把 function_call 条目的 arguments 归一化成 JSON 字符串。
// SDK 写法有三：JSON 字符串（官方）/ 对象（Codex 等直接传对象）/ 缺省。
// 对象形态此前直接 400（"cannot unmarshal object into ...arguments of type string"）。
func normalizeToolArguments(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// normalizeCustomInput 把 custom_tool_call 条目的 input 归一成 freeform 文本。
// 回程可能是 string（官方形态）或对象（SDK 直接传对象）；对象退回 JSON 串，
// 再由调用方包进 {"input":...}，与出站描述的单 input 参数形状一致。
func normalizeCustomInput(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// flattenResponsesContent 把 Responses 的 content 数组摊平成 chat 风格的 content。
// 文本：input_text/output_text → text；图片：input_image(url) → image_url；文件：
// input_file → file（file_data 可以是 data: URL、裸 base64 或 http(s) 地址）。
// file_id 不支持：本服务没有文件存储，解析不了引用。
// 非数组输入返回 ok=false，调用方走原来的 json.Marshal 透传。
func flattenResponsesContent(content any) (any, bool) {
	arr, ok := content.([]any)
	if !ok {
		return nil, false
	}
	flat := make([]map[string]any, 0, len(arr))
	for _, c := range arr {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			text, _ := m["text"].(string)
			if strings.TrimSpace(text) != "" {
				flat = append(flat, map[string]any{"type": "text", "text": text})
			}
		case "input_image":
			if url, _ := m["image_url"].(string); strings.TrimSpace(url) != "" {
				flat = append(flat, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "input_file":
			name, _ := m["filename"].(string)
			payload, _ := m["file_data"].(string)
			if strings.TrimSpace(payload) == "" {
				payload, _ = m["file_url"].(string)
			}
			if strings.TrimSpace(payload) != "" {
				flat = append(flat, map[string]any{
					"type": "file",
					"file": map[string]any{"filename": name, "file_data": payload},
				})
			}
		}
	}
	return flat, true
}

// responseObject 是非流式 Response 对象。
type responseObject struct {
	ID         string           `json:"id"`
	Object     string           `json:"object"` // "response"
	CreatedAt  int64            `json:"created_at"`
	Status     string           `json:"status"` // "completed"
	Model      string           `json:"model"`
	Output     []responseOutput `json:"output"`
	OutputText string           `json:"output_text"`
	Usage      *ResponsesUsage  `json:"usage,omitempty"`
}

// customToolNames 收集本请求里声明为 freeform（type:"custom"）的工具名。
// 客户端（Codex CLI）把 apply_patch 这类工具发成 custom；上游没有 tools 通道，
// 我们用提示词仿真并按 function 描述，回程必须还原成 custom_tool_call 它才认。
func customToolNames(tools []responsesTool) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]bool, 2)
	for _, t := range tools {
		if strings.EqualFold(t.Type, "custom") {
			// 顶层 name 为空时回退到嵌套 function.name：客户端可能按
			// {"type":"custom","function":{"name":"apply_patch"}} 声明，
			// 只读 t.Name 会漏掉，回程就还原不出 custom_tool_call。
			n := strings.TrimSpace(t.Name)
			if n == "" && t.Function != nil {
				n = strings.TrimSpace(t.Function.Name)
			}
			if n != "" {
				out[n] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// customInput 从仿真出来的 arguments 里取出 freeform 输入（取不到就原样用 arguments）。
func customInput(arguments string) string {
	var m map[string]any
	if json.Unmarshal([]byte(arguments), &m) == nil {
		if s, ok := m["input"].(string); ok {
			return s
		}
	}
	return arguments
}

// responseOutput 是 output 数组条目（message / function_call / custom_tool_call）。
type responseOutput struct {
	ID        string                `json:"id"`
	Type      string                `json:"type"` // "message" | "function_call" | "custom_tool_call"
	Status    string                `json:"status"`
	Role      string                `json:"role,omitempty"`
	Content   []responseContentPart `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
	Input     string                `json:"input,omitempty"` // custom_tool_call 的 freeform 输入
}

// responseContentPart 是 message 的 content part。
type responseContentPart struct {
	Type string `json:"type"` // "output_text" | "output_refusal"
	Text string `json:"text,omitempty"`
}

// ResponsesUsage 用量。InputTokensDetails 对应官方 responses 的 input_tokens_details，
// CachedTokens 透传 chat 层的模拟缓存读命中。
type ResponsesUsage struct {
	InputTokens        int                         `json:"input_tokens"`
	OutputTokens       int                         `json:"output_tokens"`
	TotalTokens        int                         `json:"total_tokens"`
	InputTokensDetails *ResponsesInputTokenDetails `json:"input_tokens_details,omitempty"`
}

type ResponsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// responsesUsageFromChat 把 chat Usage 映射成 Responses Usage（含缓存明细透传）。
func responsesUsageFromChat(u *Usage) *ResponsesUsage {
	if u == nil {
		return nil
	}
	out := &ResponsesUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.InputTokensDetails = &ResponsesInputTokenDetails{CachedTokens: u.PromptTokensDetails.CachedTokens}
	}
	return out
}

// openaiResponseFromChat 将 OpenAI 非流式响应转为 Response 对象。
func openaiResponseFromChat(chatResp any, custom map[string]bool) *responseObject {
	obj := &responseObject{
		ID: "resp_" + randomHex(16), Object: "response",
		CreatedAt: time.Now().Unix(), Status: "completed",
		Output: make([]responseOutput, 0),
	}
	cr, ok := chatResp.(*struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	})
	if !ok || cr == nil || len(cr.Choices) == 0 {
		return obj
	}
	m := cr.Choices[0].Message
	obj.Model = cr.Model
	item := responseOutput{
		ID: "msg_" + randomHex(16), Type: "message",
		Status: "completed", Role: "assistant",
		Content: make([]responseContentPart, 0),
	}
	if m.Reasoning != "" {
		item.Content = append(item.Content, responseContentPart{Type: "reasoning", Text: m.Reasoning})
	}
	if t := contentText(m.Content); t != "" {
		item.Content = append(item.Content, responseContentPart{Type: "output_text", Text: t})
		obj.OutputText += t
	}
	if len(item.Content) > 0 {
		obj.Output = append(obj.Output, item)
	}
	for _, tc := range m.ToolCalls {
		if custom[tc.Function.Name] {
			obj.Output = append(obj.Output, responseOutput{
				ID: "ctc_" + randomHex(8), Type: "custom_tool_call",
				Status: "completed", CallID: tc.ID, Name: tc.Function.Name,
				Input: customInput(tc.Function.Arguments),
			})
			continue
		}
		obj.Output = append(obj.Output, responseOutput{
			ID: "fc_" + randomHex(8), Type: "function_call",
			Status: "completed", CallID: tc.ID, Name: tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	if cr.Usage != nil {
		obj.Usage = responsesUsageFromChat(cr.Usage)
	}
	return obj
}

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	var req ResponsesRequest
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "invalid request body: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "model is required", "type": "invalid_request_error"},
		})
		return
	}
	chatReq, err := responsesToChatRequest(&req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	custom := customToolNames(req.Tools)
	if req.Stream {
		s.streamResponses(w, r, chatReq, custom)
		return
	}
	body, _ := json.Marshal(chatReq)
	rec := httptest.NewRecorder()
	s.handleChat(rec, internalChatRequest(r, body))
	if rec.Code != http.StatusOK {
		writeJSON(w, rec.Code, json.RawMessage(rec.Body.Bytes()))
		return
	}
	var chatResp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": "parse response: " + err.Error(), "type": "api_error"},
		})
		return
	}
	obj := openaiResponseFromChat(&chatResp, custom)
	writeJSON(w, http.StatusOK, obj)
}

// streamResponses 流式 Responses：内部跑 handleChat 写 OpenAI chunk，
// 边读边转换为 Responses SSE 事件。
// custom 是本轮声明为 freeform（type:"custom"）的工具名，回程要还原成 custom_tool_call。
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, chatReq *ChatCompletionRequest, custom map[string]bool) {
	if stormFastFail(w, r, "openai") {
		return
	}
	pr, pw := io.Pipe()
	defer pr.Close()

	body, _ := json.Marshal(chatReq)
	httpReq := internalChatRequest(r, body)

	done := make(chan struct{})
	go func() {
		defer pw.Close()
		defer close(done)
		s.handleChat(&pipeResponseWriter{w: pw}, httpReq)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	respID := "resp_" + randomHex(16)
	itemID := "msg_" + randomHex(16)
	created := time.Now().Unix()
	model := chatReq.Model

	var seq int
	emit := func(ev any) error {
		// Responses 事件按规范带自增 sequence_number；严格客户端会校验它。
		if m, ok := ev.(map[string]any); ok {
			m["sequence_number"] = seq
			seq++
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName(ev), b); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}

	// 起始事件
	_ = emit(map[string]any{
		"type": "response.created", "response_id": respID,
		"response": map[string]any{
			"id": respID, "object": "response", "created_at": created, "status": "in_progress", "model": model,
		},
	})
	_ = emit(map[string]any{"type": "response.in_progress", "response_id": respID})

	var text strings.Builder
	var reasoning strings.Builder
	type pendingFC struct {
		id, name, itemID string
		args             strings.Builder
		custom           bool // freeform（type:"custom"）工具：回程要发 custom_tool_call
	}
	var fcs []*pendingFC
	var curFC *pendingFC
	itemAdded := false
	reasoningAdded := false
	var respUsage *ResponsesUsage
	// 内核在流中失败时会带 error 标记（见 ChunkError）：这是「这一轮失败」而不是
	// 「模型说了这句话」，所以不能把它当正文发给客户端，最后要发 response.failed。
	var chunkErr *ChunkError

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(line[6:])
		if payload == "[DONE]" {
			break
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			if chunkErr == nil {
				chunkErr = chunk.Error
			}
			continue // 错误帧：不当正文/工具调用处理
		}
		if chunk.Usage != nil && respUsage == nil {
			respUsage = responsesUsageFromChat(chunk.Usage)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		// 心跳帧（空 delta）：映射成 in_progress 事件透传—— Responses 链路的
		// 客户端（codex cli）同样依赖字节流保活，见 streamChat 心跳注释。
		if d := chunk.Choices[0].Delta; d.Content == nil && d.ReasoningContent == nil && len(d.ToolCalls) == 0 {
			_ = emit(map[string]any{"type": "response.in_progress", "response_id": respID})
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != nil && *delta.Content != "" {
			if !itemAdded {
				_ = emit(map[string]any{
					"type": "response.output_item.added", "response_id": respID,
					"output_index": 0, "item": map[string]any{
						"id": itemID, "type": "message", "status": "in_progress", "role": "assistant",
					},
				})
				_ = emit(map[string]any{
					"type": "response.content_part.added", "response_id": respID,
					"item_id": itemID, "output_index": 0, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": ""},
				})
				itemAdded = true
			}
			text.WriteString(*delta.Content)
			_ = emit(map[string]any{
				"type": "response.output_text.delta", "response_id": respID,
				"item_id": itemID, "output_index": 0, "content_index": 0, "delta": *delta.Content,
			})
		}
		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			if !reasoningAdded {
				_ = emit(map[string]any{
					"type": "response.reasoning_summary_text.added", "response_id": respID,
					"output_index": 0, "item_id": itemID,
					"summary": []map[string]any{{"type": "summary_text", "text": ""}},
				})
				reasoningAdded = true
			}
			reasoning.WriteString(*delta.ReasoningContent)
			_ = emit(map[string]any{
				"type": "response.reasoning_summary_text.delta", "response_id": respID,
				"item_id": itemID, "output_index": 0, "summary_index": 0, "delta": *delta.ReasoningContent,
			})
		}
		for _, tc := range delta.ToolCalls {
			if tc.Index < len(fcs) && fcs[tc.Index] != nil {
				curFC = fcs[tc.Index]
			} else {
				curFC = &pendingFC{
					id: tc.ID, name: tc.Function.Name, itemID: "fc_" + randomHex(8),
					custom: custom[tc.Function.Name],
				}
				if curFC.custom {
					curFC.itemID = "ctc_" + randomHex(8)
				}
				fcs = append(fcs, curFC)
				fcIdx := fcOutputIndex(itemAdded, len(fcs)-1)
				itemType, itemID := "function_call", curFC.itemID
				if curFC.custom {
					itemType = "custom_tool_call"
				}
				_ = emit(map[string]any{
					"type": "response.output_item.added", "response_id": respID,
					"output_index": fcIdx, "item": map[string]any{
						"id": itemID, "type": itemType,
						"status": "in_progress", "call_id": tc.ID, "name": tc.Function.Name,
					},
				})
			}
			if tc.Function.Arguments != "" {
				curFC.args.WriteString(tc.Function.Arguments)
				evType := "response.function_call_arguments.delta"
				if curFC.custom {
					evType = "response.custom_tool_call_input.delta"
				}
				_ = emit(map[string]any{
					"type": evType, "response_id": respID,
					"item_id": curFC.itemID, "output_index": fcOutputIndex(itemAdded, len(fcs)-1), "delta": tc.Function.Arguments,
				})
			}
		}
		_ = chunk // 保留引用
	}

	// 结束事件
	if itemAdded {
		_ = emit(map[string]any{
			"type": "response.output_text.done", "response_id": respID,
			"item_id": itemID, "output_index": 0, "content_index": 0, "text": text.String(),
		})
		_ = emit(map[string]any{
			"type": "response.content_part.done", "response_id": respID,
			"item_id": itemID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text.String()},
		})
		_ = emit(map[string]any{
			"type": "response.output_item.done", "response_id": respID,
			"output_index": 0, "item": map[string]any{
				"id": itemID, "type": "message", "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text.String()}},
			},
		})
	}
	output := make([]map[string]any, 0)
	if itemAdded {
		output = append(output, map[string]any{
			"id": itemID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text.String()}},
		})
	}
	for i, fc := range fcs {
		fcIdx := fcOutputIndex(itemAdded, i)
		var item map[string]any
		if fc.custom {
			input := customInput(fc.args.String())
			_ = emit(map[string]any{
				"type": "response.custom_tool_call_input.done", "response_id": respID,
				"item_id": fc.itemID, "output_index": fcIdx, "input": input,
			})
			item = map[string]any{
				"id": fc.itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": fc.id, "name": fc.name, "input": input,
			}
		} else {
			_ = emit(map[string]any{
				"type": "response.function_call_arguments.done", "response_id": respID,
				"item_id": fc.itemID, "output_index": fcIdx, "arguments": fc.args.String(),
			})
			item = map[string]any{
				"id": fc.itemID, "type": "function_call", "status": "completed",
				"call_id": fc.id, "name": fc.name, "arguments": fc.args.String(),
			}
		}
		// Codex 靠 output_item.done 把这一项并入下一轮请求的 input；缺了它，
		// 宿主执行完工具也没有可回灌的 function_call 项，工具链就断在这里。
		_ = emit(map[string]any{
			"type": "response.output_item.done", "response_id": respID,
			"output_index": fcIdx, "item": item,
		})
		output = append(output, item)
	}
	if chunkErr != nil {
		// 这一轮失败了：客户端必须看到失败，而不是一段像模型说的话的错误文本。
		// （非流式路径由 handleChat 直接回错误状态，不走这里。）
		_ = emit(map[string]any{
			"type": "response.failed", "response_id": respID,
			"response": map[string]any{
				"id": respID, "object": "response", "created_at": created,
				"status": "failed", "model": model, "output": output,
				"error": map[string]any{
					"code":    firstNonEmptyStr(chunkErr.Type, "upstream_error"),
					"message": chunkErr.Message,
				},
			},
		})
		<-done
		return
	}
	completed := map[string]any{
		"id": respID, "object": "response", "created_at": created, "status": "completed",
		"model": model, "output": output, "output_text": text.String(),
	}
	if respUsage != nil {
		completed["usage"] = respUsage
	}
	_ = emit(map[string]any{
		"type": "response.completed", "response_id": respID, "response": completed,
	})
	<-done
}

// firstNonEmptyStr 取第一个非空字符串（空则用兜底）。
func firstNonEmptyStr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// fcOutputIndex 计算 function_call 的全局 output_index：
// 消息 item 占用 0，函数调用从 1 起；无消息 item 时从 0 起。
func fcOutputIndex(itemAdded bool, fcSeq int) int {
	if itemAdded {
		return fcSeq + 1
	}
	return fcSeq
}

// eventName 从事件对象提取 type 字段（SSE event: 头）。
func eventName(ev any) string {
	switch v := ev.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok {
			return t
		}
	}
	return "message"
}

// pipeResponseWriter 把 ResponseWriter 桥接到 io.Writer（供内部 handleChat 复用）。
type pipeResponseWriter struct {
	w io.WriteCloser
	h http.Header
}

func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeResponseWriter) WriteHeader(int)             {}
func (p *pipeResponseWriter) Header() http.Header {
	if p.h == nil {
		p.h = make(http.Header)
	}
	return p.h
}
