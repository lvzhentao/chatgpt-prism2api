package api

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"prism-2api/internal/emulation"
)

// maxEffortLevel 思考等级上限：WEB2API_MAX_EFFORT（low|medium|high|max），默认 low。
// Claude Code 2026 默认发 output_config.effort="max"（Cursor 特有的无限思考等级，
// 实测 40-181s 纯 thinking 零正文）；压到 low 后同账号稳定 6-14s 出正文。
func maxEffortLevel() string {
	s := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_MAX_EFFORT")))
	switch s {
	case "low", "medium", "high", "max":
		return s
	}
	return "low"
}

// capEffort 把客户端 effort 归一化为官方等级（low/medium/high），并应用上限。
// Cursor 特有等级（max 等）与未知等级一律按上限处理。
func capEffort(effort string) string {
	level := strings.ToLower(strings.TrimSpace(effort))
	normalized := level
	switch level {
	case "minimal", "min", "low":
		normalized = "low"
	case "medium":
		normalized = "medium"
	case "high":
		normalized = "high"
	default: // "max" 及未知等级 → 上限
		normalized = "max"
	}
	cap := maxEffortLevel()
	rank := map[string]int{"low": 1, "medium": 2, "high": 3, "max": 4}
	if rank[normalized] > rank[cap] {
		return cap
	}
	if normalized == "max" {
		return cap
	}
	return normalized
}

// maxTokensCap 输出上限：WEB2API_MAX_TOKENS_CAP 秒，默认 16000；0=不限制。
// 实测 claude-opus-4-8 在 max_tokens=64000 + adaptive thinking 下可无限思考
// （181s+ 纯 thinking 零正文）；收紧输出预算后同账号 6-12s 出正文。
func maxTokensCap() int {
	s := strings.TrimSpace(os.Getenv("WEB2API_MAX_TOKENS_CAP"))
	if s == "" {
		return 16000
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 16000
	}
	return n
}

// capMaxTokens 应用输出上限（n<=0 或未启用时原样返回）。
func capMaxTokens(n int) int {
	if n <= 0 {
		return n
	}
	if c := maxTokensCap(); c > 0 && n > c {
		return c
	}
	return n
}

// AnthropicToOpenAIRequest 将 AnthropicMessagesRequest 转换为 ChatCompletionRequest，
// 以便复用现有的 Cursor 调度层、Agent 请求构建器及账号池机制。
func AnthropicToOpenAIRequest(req *AnthropicMessagesRequest, customMap map[string]string, defaultModel string) (*ChatCompletionRequest, error) {
	out := &ChatCompletionRequest{
		Model:       CleanAnthropicModelName(req.Model, customMap, defaultModel),
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Metadata:    req.Metadata,
	}
	if req.MaxTokens > 0 {
		mt := capMaxTokens(req.MaxTokens)
		out.MaxTokens = &mt
	}
	// 思考模式与 Effort 映射（优先 output_config.effort，其次 thinking.enabled/adaptive）。
	// 客户端等级统一走 capEffort：Claude Code 2026 的 effort="max"（Cursor 无限思考）被
	// 压到 WEB2API_MAX_EFFORT（默认 low）；adaptive（无预算）同理。收紧后实测同账号
	// 稳定 6-14s 出正文，不再出现 40-181s 纯 thinking。
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		out.ReasoningEffort = capEffort(req.OutputConfig.Effort)
	} else if thinkingRequested(req.Thinking) {
		var effort string
		if req.Thinking.BudgetTokens >= 8000 {
			effort = "high"
		} else if req.Thinking.BudgetTokens >= 2000 {
			effort = "medium"
		} else {
			effort = "low"
		}
		out.ReasoningEffort = capEffort(effort)
	}

	var messages []ChatMessage

	// 1. 处理 System 提示
	systemText := extractAnthropicSystem(req.System)
	if systemText != "" {
		systemBytes, _ := json.Marshal(systemText)
		messages = append(messages, ChatMessage{
			Role:    "system",
			Content: systemBytes,
		})
	}

	// 2. 处理 Messages
	for _, msg := range req.Messages {
		// Claude Code 延迟工具模式：工具定义以无 role 消息下发（name+input_schema）。
		// 提取为工具声明，否则客户端 tools=[] 时上游工具调用会被静默丢弃。
		if msg.Role == "" && msg.Name != "" {
			if isDeferredToolDecl(msg) {
				schema := msg.InputSchema
				if len(schema) == 0 {
					schema = json.RawMessage(`{"type":"object"}`)
				}
				out.Tools = append(out.Tools, Tool{
					Type: "function",
					Function: ToolFunction{
						Name:        msg.Name,
						Description: msg.Description,
						Parameters:  schema,
					},
				})
			}
			continue
		}
		converted, err := convertAnthropicMessage(msg)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	out.Messages = messages

	// 3. 处理 Tools
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if isAnthropicServerTool(t) {
				continue
			}
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out.Tools = append(out.Tools, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  schema,
				},
			})
		}
	}

	// 4. 处理 ToolChoice
	if len(req.ToolChoice) > 0 {
		var tcMap map[string]any
		if err := json.Unmarshal(req.ToolChoice, &tcMap); err == nil {
			switch tcMap["type"] {
			case "auto":
				out.ToolChoice = "auto"
			case "any":
				out.ToolChoice = "required"
			case "none":
				out.ToolChoice = "none"
			case "tool":
				if name, ok := tcMap["name"].(string); ok && name != "" {
					tcBytes, _ := json.Marshal(map[string]any{
						"type": "function",
						"function": map[string]string{
							"name": name,
						},
					})
					out.ToolChoice = tcBytes
				}
			}
		}
	}

	return out, nil
}

// CleanAnthropicModelName 处理模型名称映射。
// 规则顺序：
// 1. 用户自定义映射（最高优先级，支持原名、小写名、去日期/版本后缀匹配）
// 2. 默认透传：未匹配时原样传递，绝不硬编码替换。
func CleanAnthropicModelName(model string, customMap map[string]string, defaultModel string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		if defaultModel != "" {
			return defaultModel
		}
		return "claude-4.5-sonnet"
	}
	lower := strings.ToLower(m)

	// 1. 用户自定义映射（最高优先级）
	if customMap != nil {
		if mapped, ok := customMap[m]; ok && mapped != "" {
			return mapped
		}
		if mapped, ok := customMap[lower]; ok && mapped != "" {
			return mapped
		}
		trimmed := strings.TrimSuffix(lower, "-latest")
		if mapped, ok := customMap[trimmed]; ok && mapped != "" {
			return mapped
		}
		reDate := regexp.MustCompile(`-\d{8}$`)
		trimmedDate := reDate.ReplaceAllString(trimmed, "")
		if mapped, ok := customMap[trimmedDate]; ok && mapped != "" {
			return mapped
		}
	}

	// 2. 无映射时 100% 原始透传
	return m
}

// extractAnthropicSystem 提取 system 字符串
func extractAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// isDeferredToolDecl 判断无 role 消息是否为 Claude Code 延迟工具定义
// （name + input_schema，无 content）。
func isDeferredToolDecl(msg AnthropicMessage) bool {
	if msg.Name == "" || len(msg.Content) > 0 {
		return false
	}
	if msg.DeferLoading {
		return true
	}
	return len(msg.InputSchema) > 0 || msg.Description != ""
}

// convertAnthropicMessage 将单条 Anthropic 消息转换为一条或多条 OpenAI 消息（例如 tool_result 展开为 role: tool）
func convertAnthropicMessage(msg AnthropicMessage) ([]ChatMessage, error) {
	if len(msg.Content) == 0 || string(msg.Content) == "null" {
		return nil, nil
	}

	// 尝试解析纯文本字符串
	var plainText string
	if err := json.Unmarshal(msg.Content, &plainText); err == nil {
		rawBytes, _ := json.Marshal(plainText)
		return []ChatMessage{{
			Role:    msg.Role,
			Content: rawBytes,
		}}, nil
	}

	// 尝试解析 Block 数组
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		// 既非纯字符串也非 Block 数组，原样保留
		return []ChatMessage{{
			Role:    msg.Role,
			Content: msg.Content,
		}}, nil
	}

	var results []ChatMessage
	var curParts []ContentPart
	var toolCalls []ToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			curParts = append(curParts, ContentPart{
				Type: "text",
				Text: b.Text,
			})
		case "document":
			if b.Source != nil {
				data := b.Source.Data
				if data == "" {
					data = b.Source.FileID
				}
				if text := emulation.ExtractDocument(b.Source.MediaType, b.Source.Type, data, b.Source.URL, b.Title); text != "" {
					curParts = append(curParts, ContentPart{Type: "text", Text: text})
				}
				// 原始文件同时上传：抽不出文字的（扫描件/图表/表格）要靠上游读项目文件，
				// 抽得出文字的也不亏——多一个 project_path 引用，模型能按需回看原件。
				if strings.TrimSpace(b.Source.Type) == "base64" && strings.TrimSpace(b.Source.Data) != "" {
					curParts = append(curParts, ContentPart{Type: "file", File: &FilePart{
						Filename: b.Title,
						FileData: dataURL(b.Source.MediaType, b.Source.Data),
					}})
				}
			}
		case "image":
			if b.Source != nil {
				url := ""
				if b.Source.Type == "base64" && b.Source.Data != "" {
					mediaType := b.Source.MediaType
					if mediaType == "" {
						mediaType = "image/png"
					}
					url = fmt.Sprintf("data:%s;base64,%s", mediaType, b.Source.Data)
				} else if b.Source.Type == "url" && b.Source.URL != "" {
					url = b.Source.URL
				}
				if url != "" {
					curParts = append(curParts, ContentPart{
						Type:     "image_url",
						ImageURL: &ImageURLPart{URL: url},
					})
				}
			}
		case "server_tool_use", "web_search_tool_result", "code_execution_tool_result",
			"bash_code_execution_tool_result", "text_editor_code_execution_tool_result",
			"mcp_tool_use", "mcp_tool_result":
			continue
		case "tool_use":
			if emulation.IsServerToolID(b.ID) {
				continue
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
		case "tool_result":
			if emulation.IsServerToolID(b.ToolUseID) {
				continue
			}
			// 如果前面累积了 user/assistant parts，先刷新
			if len(curParts) > 0 {
				contentBytes, _ := json.Marshal(curParts)
				results = append(results, ChatMessage{
					Role:    msg.Role,
					Content: contentBytes,
				})
				curParts = nil
			}
			// 单独追加一条 role: "tool"
			contentStr := extractToolResultContent(b.Content, b.IsError)
			contentBytes, _ := json.Marshal(contentStr)
			results = append(results, ChatMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    contentBytes,
			})
		}
	}

	if len(curParts) > 0 || len(toolCalls) > 0 {
		var contentBytes json.RawMessage
		if len(curParts) == 1 && curParts[0].Type == "text" {
			contentBytes, _ = json.Marshal(curParts[0].Text)
		} else if len(curParts) > 0 {
			contentBytes, _ = json.Marshal(curParts)
		}

		results = append(results, ChatMessage{
			Role:      msg.Role,
			Content:   contentBytes,
			ToolCalls: toolCalls,
		})
	}

	return results, nil
}

// dataURL 组一个 data: URL；MIME 缺失时给 application/octet-stream
// （附件解析链接管后按内容嗅探）。
func dataURL(mime, b64 string) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + strings.TrimSpace(b64)
}

// extractToolResultContent 将 tool_result 展开为文本
func extractToolResultContent(raw json.RawMessage, isError bool) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if isError {
			return "[tool error] " + s
		}
		return s
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		res := strings.Join(parts, "\n")
		if isError {
			return "[tool error] " + res
		}
		return res
	}

	if isError {
		return "[tool error] " + string(raw)
	}
	return string(raw)
}

// EstimateAnthropicTokens 快速预估 Anthropic 请求的 Token 数量（用于 count_tokens）
func EstimateAnthropicTokens(req *AnthropicMessagesRequest) int {
	if req == nil {
		return 0
	}
	totalChars := 0
	cjkChars := 0

	countStr := func(s string) {
		for _, r := range s {
			if r > 127 {
				cjkChars++
			} else {
				totalChars++
			}
		}
	}

	// 1. System
	sys := extractAnthropicSystem(req.System)
	countStr(sys)

	// 2. Messages
	for _, m := range req.Messages {
		countStr(m.Role)
		totalChars += 4 // overhead per message
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			countStr(plain)
			continue
		}
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			for _, b := range blocks {
				countStr(b.Text)
				countStr(b.Thinking)
				countStr(b.Name)
				countStr(string(b.Input))
				if b.Type == "document" && b.Source != nil {
					data := b.Source.Data
					if data == "" {
						data = b.Source.FileID
					}
					countStr(emulation.ExtractDocument(b.Source.MediaType, b.Source.Type, data, b.Source.URL, b.Title))
				}
			}
		} else {
			countStr(string(m.Content))
		}
	}

	// 3. Tools
	for _, t := range req.Tools {
		countStr(t.Name)
		countStr(t.Description)
		countStr(string(t.InputSchema))
	}

	tokens := (totalChars / 4) + int(float64(cjkChars)/1.5)
	if tokens < 1 && (totalChars > 0 || cjkChars > 0) {
		tokens = 1
	}
	return tokens
}

// RepairToolJSONArguments 修复工具调用 JSON 参数中的智能引号和常见格式错误（参考 7836246 经验）
func RepairToolJSONArguments(input string) string {
	if input == "" {
		return "{}"
	}
	// 替换智能双引号
	s := input
	s = strings.ReplaceAll(s, "“", "\"")
	s = strings.ReplaceAll(s, "”", "\"")
	s = strings.ReplaceAll(s, "‘", "'")
	s = strings.ReplaceAll(s, "’", "'")
	return s
}
