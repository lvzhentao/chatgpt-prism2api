package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// ---------- Google Gemini API（/v1beta/models/{model}:generateContent） ----------
//
// 请求：contents[{role, parts[{text|functionCall|functionResponse}]}],
// systemInstruction{parts}, generationConfig{maxOutputTokens,...}, tools[functionDeclarations]
// 响应：candidates[{content{role:model, parts}, finishReason}], usageMetadata
// 流式：streamGenerateContent（NDJSON，或 ?alt=sse 的 SSE 格式）

// geminiRequest 是 generateContent 请求体。
type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  *struct {
		MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
		Temperature     *float64 `json:"temperature,omitempty"`
		TopP            *float64 `json:"topP,omitempty"`
		StopSequences   []string `json:"stopSequences,omitempty"`
	} `json:"generationConfig,omitempty"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		} `json:"functionDeclarations,omitempty"`
	} `json:"tools,omitempty"`
}

// geminiContent 是 Content 对象。
type geminiContent struct {
	Role  string       `json:"role"` // user | model
	Parts []geminiPart `json:"parts"`
}

// geminiPart 是 Part 对象。
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

// geminiInlineData 是内联二进制（图片、PDF 等，base64）。
// fileData（远端 fileUri）不解析：本服务不代拉 Gemini Files API 的引用。
type geminiInlineData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

// geminiDefaultModel 是 gemini-* 模型名的默认映射（Cursor 目录无 gemini 模型）。
const geminiDefaultModel = "claude-4.6-sonnet"

// mapGeminiModel 映射模型名：gemini-* → 默认 cursor 模型；其余透传。
func mapGeminiModel(model string) string {
	base := model
	if idx := strings.Index(base, ":"); idx >= 0 {
		base = base[:idx]
	}
	if strings.HasPrefix(base, "gemini-") || strings.HasPrefix(base, "gemma-") {
		return geminiDefaultModel
	}
	return model
}

// geminiToChatRequest 将 Gemini 请求转为内部 ChatCompletionRequest。
func geminiToChatRequest(req *geminiRequest, model string) (*ChatCompletionRequest, error) {
	out := &ChatCompletionRequest{Model: mapGeminiModel(model), Stream: false}
	if req.GenerationConfig != nil {
		if req.GenerationConfig.MaxOutputTokens > 0 {
			mt := capMaxTokens(req.GenerationConfig.MaxOutputTokens)
			out.MaxTokens = &mt
		}
		out.Temperature = req.GenerationConfig.Temperature
		out.TopP = req.GenerationConfig.TopP
	}
	// tools → OpenAI 格式
	for _, t := range req.Tools {
		for _, fd := range t.FunctionDeclarations {
			params := json.RawMessage(`{"type":"object"}`)
			if len(fd.Parameters) > 0 {
				params = normalizeJSONSchema(fd.Parameters)
			}
			out.Tools = append(out.Tools, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        fd.Name,
					Description: fd.Description,
					Parameters:  params,
				},
			})
		}
	}

	var msgs []ChatMessage
	if req.SystemInstruction != nil {
		var texts []string
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) > 0 {
			msgs = append(msgs, ChatMessage{Role: "system", Content: json.RawMessage(mustJSON(strings.Join(texts, "\n")))})
		}
	}
	for _, c := range req.Contents {
		switch c.Role {
		case "user", "system":
			var parts []ContentPart
			for _, p := range c.Parts {
				if p.Text != "" {
					parts = append(parts, ContentPart{Type: "text", Text: p.Text})
				}
				if p.InlineData != nil && strings.TrimSpace(p.InlineData.Data) != "" {
					parts = append(parts, inlineDataPart(p.InlineData))
				}
			}
			if len(parts) > 0 {
				// 纯文本仍是字符串（老形状），带附件才升级成数组（extractFiles 认数组）。
				if len(parts) == 1 && parts[0].Type == "text" {
					msgs = append(msgs, ChatMessage{Role: "user", Content: json.RawMessage(mustJSON(parts[0].Text))})
				} else {
					msgs = append(msgs, ChatMessage{Role: "user", Content: json.RawMessage(mustJSON(parts))})
				}
			}
			// functionResponse → tool 消息（Gemini 无 call_id，用函数名配对）
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
					msgs = append(msgs, ChatMessage{
						Role: "tool", ToolCallID: p.FunctionResponse.Name,
						Content: json.RawMessage(mustJSON(string(p.FunctionResponse.Response))),
					})
				}
			}
		case "model", "assistant":
			var texts []string
			var calls []ToolCall
			for _, p := range c.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
				if p.FunctionCall != nil {
					calls = append(calls, ToolCall{
						ID: p.FunctionCall.Name, Type: "function",
						Function: ToolCallFunction{Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args)},
					})
				}
			}
			msg := ChatMessage{Role: "assistant"}
			if len(texts) > 0 {
				msg.Content = json.RawMessage(mustJSON(strings.Join(texts, "\n")))
			}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			msgs = append(msgs, msg)
		}
	}
	out.Messages = msgs
	return out, nil
}

// inlineDataPart 把 Gemini 的内联二进制转成网关内部的附件块：
// 图片走 image_url（data: URL），其余（PDF 等）走 file。
func inlineDataPart(d *geminiInlineData) ContentPart {
	mime := strings.TrimSpace(d.MimeType)
	if mime == "" {
		mime = "application/octet-stream"
	}
	url := "data:" + mime + ";base64," + strings.TrimSpace(d.Data)
	if strings.HasPrefix(mime, "image/") {
		return ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: url}}
	}
	return ContentPart{Type: "file", File: &FilePart{FileData: url}}
}

// geminiModelFromPath 从 /v1beta/models/{model}:generateContent 提取模型名。
func geminiModelFromPath(path string) (string, bool) {
	// path 形如 /v1beta/models/gemini-2.5-pro:generateContent
	idx := strings.Index(path, ":generateContent")
	idxStream := strings.Index(path, ":streamGenerateContent")
	if idx < 0 && idxStream < 0 {
		return "", false
	}
	end := idx
	if idxStream >= 0 {
		end = idxStream
	}
	rest := path[:end]
	parts := strings.Split(rest, "/")
	model := parts[len(parts)-1]
	model = strings.TrimPrefix(model, "models/")
	return model, true
}

// geminiResponse 是非流式响应。
type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
}

// geminiCandidate 是候选。
type geminiCandidate struct {
	Content      *geminiContent `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index"`
}

// geminiUsage 用量。
type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// geminiFromOpenAI 将 OpenAI 响应转为 Gemini 格式。
func geminiFromOpenAI(model string, msg *ChatMessage, usage *Usage) *geminiResponse {
	resp := &geminiResponse{Candidates: []geminiCandidate{{
		Content:      &geminiContent{Role: "model", Parts: []geminiPart{}},
		FinishReason: "STOP", Index: 0,
	}}}
	if msg == nil {
		return resp
	}
	c := resp.Candidates[0].Content
	if msg.Reasoning != "" {
		c.Parts = append(c.Parts, geminiPart{Text: msg.Reasoning})
	}
	if t := contentText(msg.Content); t != "" {
		c.Parts = append(c.Parts, geminiPart{Text: t})
	}
	for _, tc := range msg.ToolCalls {
		var args json.RawMessage
		if tc.Function.Arguments != "" {
			args = json.RawMessage(tc.Function.Arguments)
		} else {
			args = json.RawMessage("{}")
		}
		c.Parts = append(c.Parts, geminiPart{
			FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args},
		})
		resp.Candidates[0].FinishReason = "STOP"
	}
	if usage != nil {
		resp.UsageMetadata = geminiUsage{
			PromptTokenCount:     usage.PromptTokens,
			CandidatesTokenCount: usage.CompletionTokens,
			TotalTokenCount:      usage.TotalTokens,
		}
	}
	return resp
}

// handleGemini 处理 generateContent 与 streamGenerateContent。
func (s *Server) handleGemini(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	model, ok := geminiModelFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": 400, "message": "invalid endpoint path: " + r.URL.Path, "status": "INVALID_ARGUMENT"},
		})
		return
	}
	var req geminiRequest
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": 400, "message": "invalid request body: " + err.Error(), "status": "INVALID_ARGUMENT"},
		})
		return
	}
	chatReq, err := geminiToChatRequest(&req, model)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": 400, "message": err.Error(), "status": "INVALID_ARGUMENT"},
		})
		return
	}

	streaming := strings.Contains(r.URL.Path, ":streamGenerateContent")
	if streaming {
		chatReq.Stream = true
		s.streamGemini(w, r, chatReq, model)
		return
	}

	body, _ := json.Marshal(chatReq)
	rec := httptest.NewRecorder()
	s.handleChat(rec, internalChatRequest(r, body))
	if rec.Code != http.StatusOK {
		var errObj map[string]any
		json.Unmarshal(rec.Body.Bytes(), &errObj)
		msg := "upstream error"
		if errObj != nil {
			if e, ok := errObj["error"].(map[string]any); ok {
				msg, _ = e["message"].(string)
			}
		}
		writeJSON(w, rec.Code, map[string]any{
			"error": map[string]any{"code": rec.Code, "message": msg, "status": "UNAVAILABLE"},
		})
		return
	}
	var chatResp struct {
		Choices []struct {
			Message ChatMessage `json:"message"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "parse response: " + err.Error(), "status": "INTERNAL"},
		})
		return
	}
	var msg *ChatMessage
	if len(chatResp.Choices) > 0 {
		msg = &chatResp.Choices[0].Message
	}
	writeJSON(w, http.StatusOK, geminiFromOpenAI(model, msg, chatResp.Usage))
}

// streamGemini 流式 Gemini：内部跑 handleChat 写 OpenAI chunk，边读边转 Gemini SSE/NDJSON。
func (s *Server) streamGemini(w http.ResponseWriter, r *http.Request, chatReq *ChatCompletionRequest, model string) {
	if stormFastFail(w, r, "gemini") {
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

	useSSE := r.URL.Query().Get("alt") == "sse"
	if useSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	writeChunk := func(part geminiResponse) error {
		b, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if useSSE {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return err
			}
		} else {
			if _, err := w.Write(append(b, '\n')); err != nil {
				return err
			}
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}

	var text strings.Builder
	var reasoning strings.Builder
	var funcCalls []*geminiFunctionCall
	usage := &Usage{}

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
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != nil && *delta.Content != "" {
			text.WriteString(*delta.Content)
			_ = writeChunk(geminiResponse{Candidates: []geminiCandidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{{Text: *delta.Content}}},
			}}})
		}
		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			reasoning.WriteString(*delta.ReasoningContent)
			_ = writeChunk(geminiResponse{Candidates: []geminiCandidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{{Text: *delta.ReasoningContent}}},
			}}})
		}
		for _, tc := range delta.ToolCalls {
			fc := &geminiFunctionCall{Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)}
			funcCalls = append(funcCalls, fc)
			_ = writeChunk(geminiResponse{Candidates: []geminiCandidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{{FunctionCall: fc}}},
			}}})
		}
	}

	// 结束块：finishReason + usageMetadata
	final := geminiResponse{Candidates: []geminiCandidate{{
		Content:      &geminiContent{Role: "model", Parts: []geminiPart{}},
		FinishReason: "STOP", Index: 0,
	}}}
	if text.Len() > 0 || reasoning.Len() > 0 {
		if reasoning.Len() > 0 {
			final.Candidates[0].Content.Parts = append(final.Candidates[0].Content.Parts, geminiPart{Text: reasoning.String()})
		}
		if text.Len() > 0 {
			final.Candidates[0].Content.Parts = append(final.Candidates[0].Content.Parts, geminiPart{Text: text.String()})
		}
	}
	for _, fc := range funcCalls {
		final.Candidates[0].Content.Parts = append(final.Candidates[0].Content.Parts, geminiPart{FunctionCall: fc})
	}
	final.UsageMetadata = geminiUsage{
		PromptTokenCount:     usage.PromptTokens,
		CandidatesTokenCount: usage.CompletionTokens,
		TotalTokenCount:      usage.TotalTokens,
	}
	_ = writeChunk(final)
	<-done
}

// handleGeminiModels 处理 GET /v1beta/models（模型列表）。
func (s *Server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	ids := s.catalog.PublicModelIDs()
	if ids == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "load models failed", "status": "UNAVAILABLE"},
		})
		return
	}
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{
			"name": "models/" + id, "displayName": id,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
