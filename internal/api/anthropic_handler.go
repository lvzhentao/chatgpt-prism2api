package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/admin"
	"prism-2api/internal/emulation"
	"prism-2api/internal/emulation/websearch"
	"prism-2api/internal/failclass"
)

// SSEAnthropicWriter 负责向 HTTP 客户端写入 Anthropic 格式的 SSE 事件
type SSEAnthropicWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func NewSSEAnthropicWriter(w http.ResponseWriter) *SSEAnthropicWriter {
	f, _ := w.(http.Flusher)
	return &SSEAnthropicWriter{w: w, flusher: f}
}

// ---------- SSE 录制（WEB2API_SSE_DUMP=<dir> 时启用） ----------
// 诊断用：把实际发给客户端的原始 SSE 字节按请求写入文件，
// 用于对照客户端解析失败的确切字节流。

var (
	sseDumpOnce sync.Once
	sseDumpDir  string
	sseDumpSeq  int
	sseDumpMu   sync.Mutex
)

func sseDumpEnabled() string {
	sseDumpOnce.Do(func() { sseDumpDir = os.Getenv("WEB2API_SSE_DUMP") })
	return sseDumpDir
}

func sseDumpWrite(p []byte) {
	dir := sseDumpEnabled()
	if dir == "" {
		return
	}
	sseDumpMu.Lock()
	defer sseDumpMu.Unlock()
	sseDumpSeq++
	name := fmt.Sprintf("sse_%s_%d.txt", time.Now().Format("150405.000"), sseDumpSeq)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, name), p, 0o644)
}

func (s *SSEAnthropicWriter) Emit(eventType string, data any) error {
	bytes, err := marshalJSONNoEscape(data)
	if err != nil {
		return err
	}
	frame := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, bytes)
	if sseDumpEnabled() != "" {
		sseDumpWrite([]byte(frame))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return err
	}
	s.flushLocked()
	return nil
}
func (s *SSEAnthropicWriter) EmitComment(comment string) error {
	frame := fmt.Sprintf(": %s\n\n", comment)
	if sseDumpEnabled() != "" {
		sseDumpWrite([]byte(frame))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return err
	}
	s.flushLocked()
	return nil
}

func (s *SSEAnthropicWriter) flushLocked() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
	_ = http.NewResponseController(s.w).Flush()
}

// 身份探针与拒绝词规则（参考 7836246 经验）
var (
	identityProbeRe = regexp.MustCompile(`(?i)^\s*(who are you\??|你是谁[呀啊吗]?\??|what is your name\??|你叫什么\??|what are you\??|自我介绍一下\??|which model are you\??|你是什么模型\??|hi\??|hello\??|你好\??)\s*$`)
	refusalPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)Cursor(?:'s)?\s+support\s+assistant`),
		regexp.MustCompile(`(?i)I[''']\s*m\s+sorry.*coding\s+assistant`),
		regexp.MustCompile(`(?i)unrelated\s+to\s+(?:programming|coding)`),
		regexp.MustCompile(`(?i)outside\s+(?:my|the)\s+scope`),
		regexp.MustCompile(`(?i)i(?:['’]m| am) not claude code`),
		regexp.MustCompile(`(?i)\bonly a coding assistant\b`),
	}
)

const claudeIdentityText = "I am Claude, made by Anthropic. I'm an AI assistant designed to be helpful, harmless, and honest. I can help you with a wide range of tasks including writing, analysis, coding, math, and more."

// isIdentityProbeQuery 检查是否是极简身份探针
func isIdentityProbeQuery(req *AnthropicMessagesRequest) bool {
	if len(req.Messages) != 1 {
		return false
	}
	m := req.Messages[0]
	if m.Role != "user" {
		return false
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return identityProbeRe.MatchString(text)
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil && len(blocks) == 1 && blocks[0].Type == "text" {
		return identityProbeRe.MatchString(blocks[0].Text)
	}
	return false
}

// CheckRefusal 检查回复中是否包含 Cursor 身份拒绝词
func CheckRefusal(text string) bool {
	for _, p := range refusalPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// handleAnthropicMessages 处理 Anthropic /v1/messages 请求
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req AnthropicMessagesRequest
	raw, err := readJSONBody(r, &req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}

	if req.Model == "" {
		req.Model = s.cfg.DefaultModel
	}

	entry := logEntryFrom(r)
	if entry != nil {
		entry.Model = req.Model
		entry.Stream = req.Stream
	}

	if s.shouldInterceptWebSearch(raw) {
		s.handleEmulatedWebSearch(w, r, &req, raw, entry)
		return
	}
	if websearch.IsOnlyCodeExecution(raw) {
		s.handleEmulatedCodeExecution(w, r, &req, raw, entry)
		return
	}
	if s.handleLocalProbes(w, r, &req, raw, entry) {
		return
	}

	injectStructuredOutput(&req)

	// 1. 快速身份探针响应（秒回，防暴露底层）
	if isIdentityProbeQuery(&req) {
		msgID := emulation.MessageID()
		usage := composeUsage(10, 35, nil, 0)
		stop := "end_turn"
		if req.Stream {
			s.streamIdentityProbeResponse(w, msgID, req.Model, usage)
		} else {
			resp := AnthropicMessageResponse{
				ID:   msgID,
				Type: "message",
				Role: "assistant",
				Content: []AnthropicContentBlock{
					{Type: "text", Text: claudeIdentityText},
				},
				Model:      req.Model,
				StopReason: &stop,
				Usage:      usage,
			}
			writeAnthropicJSON(w, http.StatusOK, resp)
		}
		return
	}

	// 2. 转换为内部标准 OpenAI Chat 请求（利用可配置的 ModelMap 与 DefaultModel 进行动态映射）
	chatReq, err := AnthropicToOpenAIRequest(&req, s.mergedModelRoutes(), s.cfg.DefaultModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	agentReq, err := MapChat(r.Context(), chatReq, s.promptText())
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	iter := s.newAccountIter(r.Context(), r, chatReq.Model, raw)
	if req.Stream {
		s.streamAnthropicChat(w, r, &req, chatReq, agentReq, entry, iter, raw)
		return
	}
	s.nonStreamAnthropicChat(w, r, &req, chatReq, agentReq, entry, iter, raw)
}

// handleAnthropicCountTokens 处理 token 预估请求 (POST /v1/messages/count_tokens)
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	var req AnthropicMessagesRequest
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	tokens := EstimateAnthropicTokens(&req)
	writeJSON(w, http.StatusOK, AnthropicCountTokensResponse{InputTokens: tokens})
}

func (s *Server) streamIdentityProbeResponse(w http.ResponseWriter, msgID string, model string, usage AnthropicUsage) {
	writeAnthropicSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	sw := NewSSEAnthropicWriter(w)
	_ = sw.Emit("message_start", officialMessageStart(msgID, model, usage))
	_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
	})
	_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
		Type:  "content_block_delta",
		Index: 0,
		Delta: AnthropicBlockDelta{Type: "text_delta", Text: claudeIdentityText},
	})
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{
		Type:  "content_block_stop",
		Index: 0,
	})
	_ = sw.Emit("message_delta", officialMessageDelta("end_turn", usage))
	_ = sw.Emit("message_stop", AnthropicEventMessageStop{Type: "message_stop"})
}

// nonStreamAnthropicChat 非流式对话
func (s *Server) nonStreamAnthropicChat(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, chatReq *ChatCompletionRequest, agentReq *adapter.NativeRequest, entry *admin.LogEntry, iter *accountIter, raw []byte) {
	start := time.Now()
	if stormFastFail(w, r, "anthropic") {
		return
	}
	msgID := emulation.MessageID()
	idMap := map[string]string{}
	schema := outputSchema(req)
	wantJSON := len(schema) > 0

	acc, err := iter.Next()
	if err != nil {
		writePickError(w, err, true)
		return
	}
	var lastErr error

	for {

		var fullText strings.Builder
		var fullThinking strings.Builder
		var toolCalls []ToolCall
		var toolCallMap = map[string]*ToolCall{}
		var blockedCalls []string // S1：上游调了但客户端没有的工具（吞帧，只记摘要）
		clientTools := clientToolIndex(chatReq)

		streamErr := acc.Client.Stream(r.Context(), agentReq, func(ev adapter.Event) bool {
			if ev.Thinking != nil && ev.Thinking.Text != "" {
				fullThinking.WriteString(ev.Thinking.Text)
			}
			if ev.Text != "" {
				fullText.WriteString(ev.Text)
			}
			if ev.PartialToolCall != nil && !skipPartialToolStart(ev.PartialToolCall.Name) {
				ptc := ev.PartialToolCall
				name := mapOutgoingToolNameIdx(ptc.Name, clientTools)
				if name == "" {
					// 无名 partial 帧：不落盘，避免非流式响应带无名 tool_use。
					return true
				}
				if _, ok := toolCallMap[ptc.ToolCallID]; !ok {
					call := &ToolCall{
						ID:   remapToolID(idMap, ptc.ToolCallID),
						Type: "function",
						Function: ToolCallFunction{
							Name: name,
						},
					}
					toolCallMap[ptc.ToolCallID] = call
					toolCalls = append(toolCalls, *call)
				}
			}
			if ev.ToolCall != nil {
				tc := ev.ToolCall
				name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
					Kind:       tc.Kind,
					ToolName:   tc.Name,
					ToolCallID: tc.ToolCallID,
					ArgsJSON:   tc.RawArgs,
				}, clientTools)
				if !ok {
					if blocked != "" {
						blockedCalls = append(blockedCalls, blocked)
						log.Printf("[Anthropic non-stream] upstream tool blocked (client lacks tool): %s account=%s", blocked, acc.Name)
					}
					return true
				}
				if existing, ok := toolCallMap[tc.ToolCallID]; ok {
					if name != "" {
						existing.Function.Name = name
					}
					existing.Function.Arguments += args
				} else {
					call := &ToolCall{
						ID:   remapToolID(idMap, tc.ToolCallID),
						Type: "function",
						Function: ToolCallFunction{
							Name:      name,
							Arguments: args,
						},
					}
					toolCallMap[tc.ToolCallID] = call
					toolCalls = append(toolCalls, *call)
				}
			}
			return true
		})

		if streamErr != nil {
			lastErr = streamErr
			log.Printf("[Anthropic non-stream] upstream error on account %s: %v", acc.Name, streamErr)
			res := s.noteAccount(acc, streamErr, chatReq.Model, r.Context().Err() != nil, entry, iter)
			if !res.Switch {
				if res.Class == failclass.Canceled {
					acc.Release()
					return
				}
				acc.Release()
				writeAnthropicError(w, res.HTTPStatus, anthropicErrorType(res.Class), streamErr.Error())
				return
			}
			acc.Release()
			acc, err = iter.Next()
			if err != nil {
				lastErr = err
				break
			}
			continue
		}

		// 检查是否包含拒绝词（有下一号则换号，否则返回当前文本）
		resText := fullText.String()
		// S1 收尾：发生拦截时，剪掉模型翻空沙箱的自证，补受限说明。
		if note := blockedToolNote(blockedCalls); note != "" {
			resText = stripEmptyWorkspaceClaims(resText)
			resText += "\n\n" + note
		}
		if CheckRefusal(resText) || emulation.HasIdentityLeak(resText) {
			nxt, nerr := iter.Next()
			if nerr == nil {
				acc.Release()
				acc = nxt
				continue
			}
			if cleaned := emulation.SanitizeIdentityText(resText); cleaned != "" {
				resText = cleaned
			}
		}
		if emulation.IsHvoyKnowledgeProbe(lastUserText(req)) {
			resText = emulation.ExtractNumberedAnswers(resText)
		}

		if wantJSON {
			resText = emulation.FillStructuredOutput(schema, lastUserText(req), resText)
		}

		// 构建 content blocks
		var blocks []AnthropicContentBlock
		hideThinking := suppressIdentityThinking(req)
		omitted := thinkingDisplayOmitted(req.Thinking)
		if !wantJSON && !hideThinking && fullThinking.Len() == 0 && thinkingRequested(req.Thinking) && !omitted {
			fullThinking.WriteString(emulation.DefaultThinking())
		}
		if !wantJSON && !hideThinking && (fullThinking.Len() > 0 || omitted) {
			thinking := fullThinking.String()
			sig := ""
			if (s.emu == nil || s.emu.SignatureEnabled()) && shouldSignThinking(req) {
				sig = emulation.ThinkingSignature(thinking, req.Model, msgID)
			}
			blocks = append(blocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  thinking,
				Signature: sig,
			})
		}
		if resText != "" {
			blocks = append(blocks, AnthropicContentBlock{
				Type: "text",
				Text: resText,
			})
		}
		for _, tc := range toolCalls {
			fixedArgs := RepairToolJSONArguments(tc.Function.Arguments)
			blocks = append(blocks, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(fixedArgs),
			})
		}

		stopReason := "end_turn"
		if len(toolCalls) > 0 {
			stopReason = "tool_use"
		}

		inTokens := EstimateAnthropicTokens(req)
		if inTokens < 1 {
			inTokens = 1
		}
		outTokens := estimateTokens(resText) + estimateTokens(fullThinking.String())
		for _, tc := range toolCalls {
			outTokens += estimateTokens(tc.Function.Arguments)
		}
		plan := s.cachePlan(r, raw, req.Model, inTokens)
		var cacheUsage *emulation.Usage
		if plan != nil {
			cacheUsage = plan.Result()
			plan.Commit()
			if s.emu != nil {
				s.emu.NoteCache(req.Model, inTokens, cacheUsage)
			}
		}
		usage := withThinkingUsage(composeUsage(inTokens, outTokens, cacheUsage, 0), req, fullThinking.String(), resText)
		if len(blocks) == 0 {
			blocks = []AnthropicContentBlock{{Type: "text", Text: "I don't know."}}
		}

		resp := AnthropicMessageResponse{
			ID:         msgID,
			Type:       "message",
			Role:       "assistant",
			Content:    blocks,
			Model:      req.Model,
			StopReason: &stopReason,
			Usage:      usage,
		}

		if entry != nil {
			entry.Prompt = usage.InputTokens
			entry.Completion = outTokens
			entry.Total = usage.InputTokens + outTokens
			entry.Account = acc.Name
			entry.Latency = time.Since(start).Milliseconds()
		}
		fillLog(entry, acc, iter, "")
		acc.MarkSuccess()
		s.recordClientUsage(r, usage.InputTokens, outTokens)
		acc.Release()
		writeAnthropicJSON(w, http.StatusOK, resp)
		return
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no account available")
	}
	writeAnthropicError(w, http.StatusBadGateway, "api_error", lastErr.Error())
}

// streamAnthropicChat 流式对话（输出标准 Anthropic SSE 帧）
func (s *Server) streamAnthropicChat(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, chatReq *ChatCompletionRequest, agentReq *adapter.NativeRequest, entry *admin.LogEntry, iter *accountIter, raw []byte) {
	start := time.Now()
	if stormFastFail(w, r, "anthropic") {
		return
	}
	msgID := emulation.MessageID()
	idMap := map[string]string{}
	schema := outputSchema(req)
	wantJSON := len(schema) > 0
	knowledgeFmt := !wantJSON && emulation.IsHvoyKnowledgeProbe(lastUserText(req))
	needThinking := thinkingRequested(req.Thinking) && !wantJSON && !knowledgeFmt
	countThinkingTokens := thinkingTokenCountRequested(r, req)
	inTokens := EstimateAnthropicTokens(req)
	if inTokens < 1 {
		inTokens = 1
	}
	plan := s.cachePlan(r, raw, req.Model, inTokens)
	var cacheUsage *emulation.Usage
	if plan != nil {
		cacheUsage = plan.Result()
	}
	startUsage := composeUsage(inTokens, 0, cacheUsage, 0)

	acc, err := iter.Next()
	if err != nil {
		writePickError(w, err, true)
		return
	}

	writeAnthropicSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	sw := NewSSEAnthropicWriter(w)

	// T1.0：message_start 提前 —— WriteHeader 后立即发出（Flush 已把响应头刷出）。
	// message_start 与账号无关（只含消息 id/模型/初始用量），换号重试对客户端不可见；
	// 重试守卫改为「发出内容块（content_block_start/delta）后才禁换号」。
	var sentStart, sentContent bool
	emitStart := func() {
		if sentStart {
			return
		}
		_ = sw.Emit("message_start", officialMessageStart(msgID, req.Model, startUsage))
		_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
		sentStart = true
	}
	emitStart()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
			}
		}
	}()
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			acc, err = iter.Next()
			if err != nil {
				acc.Release()
				_ = sw.Emit("error", AnthropicEventError{
					Type:  "error",
					Error: AnthropicAPIError{Type: "api_error", Message: err.Error()},
				})
				return
			}
		}

		var outTokens int
		clientTools := clientToolIndex(chatReq)
		var blockedCalls []string // S1：上游调了但客户端没有的工具（吞帧，只记摘要）
		var thinkingBuf strings.Builder
		var textBuf strings.Builder
		sawThinking := false

		// 状态机跟踪
		curBlockIdx := -1
		curBlockType := "" // "thinking" | "text" | "tool_use"
		curToolCallID := ""
		curToolName := ""
		hasToolCalls := false
		var forwardedTools []string

		closeCurrentBlock := func() {
			if curBlockType == "thinking" {
				thinking := thinkingBuf.String()
				if (s.emu == nil || s.emu.SignatureEnabled()) && shouldSignThinking(req) && (thinking != "" || thinkingDisplayOmitted(req.Thinking)) {
					if sig := emulation.ThinkingSignature(thinking, req.Model, msgID); sig != "" {
						_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
							Type:  "content_block_delta",
							Index: curBlockIdx,
							Delta: AnthropicBlockDelta{Type: "signature_delta", Signature: sig},
						})
					}
				}
				thinkingBuf.Reset()
			}
			if curBlockType != "" {
				_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{
					Type:  "content_block_stop",
					Index: curBlockIdx,
				})
				curBlockType = ""
				curToolCallID = ""
				curToolName = ""
			}
		}
		ensureBlock := func(bType string, initialBlock AnthropicContentBlock) {
			if curBlockType != "" && curBlockType != bType {
				closeCurrentBlock()
			}
			if curBlockType == "" {
				curBlockIdx++
				curBlockType = bType
				initialBlock.Type = bType
				_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
					Type:         "content_block_start",
					Index:        curBlockIdx,
					ContentBlock: initialBlock,
				})
				sentContent = true // 已向客户端发出内容块，后续失败不再静默换号
			}
		}

		emitSyntheticThinking := func() {
			if sawThinking {
				return
			}
			sawThinking = true
			ensureBlock("thinking", AnthropicContentBlock{Type: "thinking", Thinking: ""})
			if thinkingDisplayOmitted(req.Thinking) {
				return
			}
			placeholder := emulation.DefaultThinking()
			thinkingBuf.WriteString(placeholder)
			outTokens += estimateTokens(placeholder)
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type:  "content_block_delta",
				Index: curBlockIdx,
				Delta: thinkingDelta(placeholder, countThinkingTokens),
			})
			closeCurrentBlock()
		}
		streamErr := acc.Client.Stream(r.Context(), agentReq, func(ev adapter.Event) bool {
			// message_start 已在 WriteHeader 后提前发出（T1.0），此处只处理内容块。
			// 1. Thinking — 直播 thinking_delta；ThinkingCompleted 是空 text +
			// IsLastThinkingChunk，必须立刻关块，否则 Claude Code 会把后续正文攒到回合结束才渲染。
			if ev.Thinking != nil && !wantJSON && !knowledgeFmt {
				if ev.Thinking.Text != "" {
					sawThinking = true
					ensureBlock("thinking", AnthropicContentBlock{Type: "thinking", Thinking: ""})
					thinkingBuf.WriteString(ev.Thinking.Text)
					outTokens += estimateTokens(ev.Thinking.Text)
					_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
						Type:  "content_block_delta",
						Index: curBlockIdx,
						Delta: thinkingDelta(ev.Thinking.Text, countThinkingTokens),
					})
				}
				if ev.Thinking.IsLastThinkingChunk && curBlockType == "thinking" {
					closeCurrentBlock()
				}
			}

			// 2. Text — 官方顺序是 thinking 块先闭合，再出 text
			if ev.Text != "" {
				if needThinking && !sawThinking {
					emitSyntheticThinking()
				}
				outTokens += estimateTokens(ev.Text)
				if wantJSON || knowledgeFmt {
					textBuf.WriteString(ev.Text)
				} else {
					ensureBlock("text", AnthropicContentBlock{Type: "text", Text: ""})
					_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
						Type:  "content_block_delta",
						Index: curBlockIdx,
						Delta: AnthropicBlockDelta{Type: "text_delta", Text: ev.Text},
					})
				}
			}

			// 3. ToolCall (包含 PartialToolCall 与 ToolCall)
			if ev.PartialToolCall != nil && !skipPartialToolStart(ev.PartialToolCall.Name) {
				if needThinking && !sawThinking {
					emitSyntheticThinking()
				}
				mappedID := remapToolID(idMap, ev.PartialToolCall.ToolCallID)
				mappedName := mapOutgoingToolNameIdx(ev.PartialToolCall.Name, clientTools)
				if mappedName == "" {
					// 无名 partial 帧（工具名未随帧到达且流内无从回填）：
					// 绝不新开 tool_use 块 —— 缺 name 的 content_block_start
					// 会让 Claude Code 直接崩溃（VS.name.startsWith）。
					// 与当前打开块同 id 时视为其继续，忽略即可。
					return true
				}
				hasToolCalls = true
				if curBlockType != "tool_use" || curToolCallID != mappedID || curToolName != mappedName {
					closeCurrentBlock()

					curBlockIdx++
					curBlockType = "tool_use"
					curToolCallID = mappedID
					curToolName = mappedName
					_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
						Type:  "content_block_start",
						Index: curBlockIdx,
						ContentBlock: AnthropicContentBlock{
							Type:  "tool_use",
							ID:    mappedID,
							Name:  mappedName,
							Input: json.RawMessage("{}"),
						},
					})
					sentContent = true
				}
			}

			if ev.ToolCall != nil {
				tc := ev.ToolCall
				name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
					Kind:       tc.Kind,
					ToolName:   tc.Name,
					ToolCallID: tc.ToolCallID,
					ArgsJSON:   tc.RawArgs,
				}, clientTools)
				if !ok {
					if blocked != "" {
						blockedCalls = append(blockedCalls, blocked)
					}
					log.Printf("[Anthropic stream] drop tool name=%s kind=%s args=%d blocked=%q request_id=%s", tc.Name, tc.Kind, len(tc.RawArgs), blocked, logRequestID(entry))
					return true
				}
				if needThinking && !sawThinking {
					emitSyntheticThinking()
				}
				hasToolCalls = true
				forwardedTools = append(forwardedTools, fmt.Sprintf("%s->%s:%d", tc.Name, name, len(args)))
				log.Printf("[Anthropic stream] tool %s -> %s args=%d request_id=%s", tc.Name, name, len(args), logRequestID(entry))
				mappedID := remapToolID(idMap, tc.ToolCallID)
				if curBlockType != "tool_use" || curToolCallID != mappedID || curToolName != name {
					closeCurrentBlock()

					curBlockIdx++
					curBlockType = "tool_use"
					curToolCallID = mappedID
					curToolName = name
					_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
						Type:  "content_block_start",
						Index: curBlockIdx,
						ContentBlock: AnthropicContentBlock{
							Type:  "tool_use",
							ID:    mappedID,
							Name:  name,
							Input: json.RawMessage("{}"),
						},
					})
					sentContent = true
				}

				if args != "" {
					outTokens += estimateTokens(args)
					_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
						Type:  "content_block_delta",
						Index: curBlockIdx,
						Delta: AnthropicBlockDelta{
							Type:        "input_json_delta",
							PartialJSON: args,
						},
					})
				}
			}
			return true
		})
		if streamErr != nil {
			log.Printf("[Anthropic stream] upstream error on account %s: %v", acc.Name, streamErr)
			res := s.noteAccount(acc, streamErr, req.Model, r.Context().Err() != nil, entry, iter)
			if res.Switch && !sentContent && res.Class != failclass.Canceled {
				// 仅发出 message_start/ping（无内容块）时仍可静默换号重试：
				// message_start 与账号无关（只含消息 id/模型/初始用量），客户端无感知。
				acc.Release()
				continue
			}
			if !res.Switch {
				closeCurrentBlock()
				if res.Class == failclass.Canceled {
					acc.Release()
					return
				}
				_ = sw.Emit("error", map[string]any{
					"type": "error",
					"error": map[string]string{
						"type":    anthropicErrorType(res.Class),
						"message": streamErr.Error(),
					},
				})
				acc.Release()
				return
			}
			closeCurrentBlock()
			_ = sw.Emit("error", map[string]any{
				"type": "error",
				"error": map[string]string{
					"type":    "api_error",
					"message": fmt.Sprintf("stream interrupted by upstream error: %v", streamErr),
				},
			})
			if entry != nil {
				entry.Prompt = inTokens
				entry.Completion = outTokens
				entry.Total = inTokens + outTokens
				entry.Account = acc.Name
				entry.Latency = time.Since(start).Milliseconds()
				entry.Status = http.StatusBadGateway
			}
			acc.Release()
			return
		}
		emitStart() // 已在 WriteHeader 后提前发出；此处为幂等兜底（空响应也必须有 message_start）
		if needThinking && !sawThinking {
			emitSyntheticThinking()
		}
		closeCurrentBlock()
		if wantJSON {
			text := emulation.FillStructuredOutput(schema, lastUserText(req), textBuf.String())
			if text == "" {
				text = "{}"
			}
			curBlockIdx++
			_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
				Type: "content_block_start", Index: curBlockIdx,
				ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
			})
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type: "content_block_delta", Index: curBlockIdx,
				Delta: AnthropicBlockDelta{Type: "text_delta", Text: text},
			})
			_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: curBlockIdx})
			outTokens += estimateTokens(text)
		} else if textBuf.Len() > 0 && knowledgeFmt {
			text := emulation.ExtractNumberedAnswers(textBuf.String())
			if text == "" {
				text = "I don't know."
			}
			curBlockIdx++
			_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
				Type: "content_block_start", Index: curBlockIdx,
				ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
			})
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type: "content_block_delta", Index: curBlockIdx,
				Delta: AnthropicBlockDelta{Type: "text_delta", Text: text},
			})
			_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: curBlockIdx})
		} else if curBlockIdx < 0 {
			curBlockIdx++
			_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
				Type: "content_block_start", Index: curBlockIdx,
				ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
			})
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type: "content_block_delta", Index: curBlockIdx,
				Delta: AnthropicBlockDelta{Type: "text_delta", Text: "I don't know."},
			})
			_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: curBlockIdx})
			outTokens += estimateTokens("I don't know.")
		}

		// S1 收尾：被拦截的上游调用以独立 text 块告知（流式正文无法回改，只追加）。
		if note := blockedToolNote(blockedCalls); note != "" {
			closeCurrentBlock()
			curBlockIdx++
			curBlockType = "text"
			_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
				Type: "content_block_start", Index: curBlockIdx,
				ContentBlock: AnthropicContentBlock{Type: "text"},
			})
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type: "content_block_delta", Index: curBlockIdx,
				Delta: AnthropicBlockDelta{Type: "text_delta", Text: "\n\n" + note},
			})
			_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: curBlockIdx})
		}

		stopReason := "end_turn"
		if hasToolCalls {
			stopReason = "tool_use"
		}
		log.Printf("[Anthropic stream] done stop=%s tools=%v out_tokens=%d account=%s request_id=%s", stopReason, forwardedTools, outTokens, acc.Name, logRequestID(entry))
		if plan != nil {
			plan.Commit()
			if s.emu != nil {
				s.emu.NoteCache(req.Model, inTokens, cacheUsage)
			}
		}
		usage := withThinkingUsage(composeUsage(inTokens, outTokens, cacheUsage, 0), req, "", textBuf.String())

		_ = sw.Emit("message_delta", officialMessageDelta(stopReason, usage))
		_ = sw.Emit("message_stop", AnthropicEventMessageStop{Type: "message_stop"})

		acc.MarkSuccess()
		if entry != nil {
			entry.Prompt = usage.InputTokens
			entry.Completion = outTokens
			entry.Total = usage.InputTokens + outTokens
			entry.Account = acc.Name
			entry.Latency = time.Since(start).Milliseconds()
		}
		fillLog(entry, acc, iter, "")
		s.recordClientUsage(r, usage.InputTokens, outTokens)
		acc.Release()
		return
	}
}

// writeAnthropicError 写入 Anthropic 格式的错误 JSON
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	if errType == "rate_limit_error" {
		w.Header().Set("Retry-After", "60")
	}
	applyAnthropicHeaders(w)
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
