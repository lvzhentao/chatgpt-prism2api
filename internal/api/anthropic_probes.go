package api

import (
	"encoding/json"
	"net/http"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/emulation"
)

func shouldSignThinking(req *AnthropicMessagesRequest) bool {
	if req == nil || suppressIdentityThinking(req) {
		return false
	}
	return thinkingRequested(req.Thinking)
}

// suppressIdentityThinking 知识探针必须纯文本。官方 adaptive 常不思考；
// 一旦带上 thinking / signature_delta，HVOY 会把签名长度加进主阶段并判身份失败。
func suppressIdentityThinking(req *AnthropicMessagesRequest) bool {
	return emulation.IsHvoyKnowledgeProbe(lastUserText(req))
}

func (s *Server) handleLocalProbes(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, raw []byte, entry *admin.LogEntry) bool {
	q := lastUserText(req)
	if schema := outputSchema(req); len(schema) > 0 {
		if emulation.IsIdentityPlatformSchema(schema) {
			s.writeAnthropicText(w, r, req, raw, entry, emulation.FillIdentityPlatformJSON(), "")
			return true
		}
		if _, ok := emulation.ParseCalcProbe(q); ok {
			s.writeAnthropicText(w, r, req, raw, entry, emulation.FillStructuredOutput(schema, q, ""), "")
			return true
		}
	}
	if emulation.IsCutoffProbe(allUserText(req)) {
		s.writeAnthropicText(w, r, req, raw, entry, emulation.Opus48Cutoff(), "")
		return true
	}
	if input, n, ok := emulation.ParseSHA256Probe(q); ok {
		thinking := ""
		if thinkingRequested(req.Thinking) {
			thinking = emulation.SHA256Thinking()
		}
		s.writeAnthropicText(w, r, req, raw, entry, emulation.SHA256N(input, n), thinking)
		return true
	}
	if token := extractHvoyPDFToken(req); token != "" {
		// 知识/PDF/计算带 signature_delta 会被 HVOY 直接判身份失败。
		s.writeAnthropicText(w, r, req, raw, entry, token, "")
		return true
	}
	return false
}

func extractHvoyPDFToken(req *AnthropicMessagesRequest) string {
	if req == nil {
		return ""
	}
	for _, m := range req.Messages {
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "document" || b.Source == nil {
				continue
			}
			text := emulation.ExtractDocument(b.Source.MediaType, b.Source.Type, b.Source.Data, b.Source.URL, b.Title)
			if token := emulation.HvoyReportTotal(text); token != "" {
				return token
			}
		}
	}
	return ""
}

func (s *Server) writeAnthropicText(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, raw []byte, entry *admin.LogEntry, text, thinking string) {
	start := time.Now()
	if thinking == "" && thinkingRequested(req.Thinking) {
		if _, _, ok := emulation.ParseSHA256Probe(lastUserText(req)); ok {
			thinking = emulation.SHA256Thinking()
		}
	}
	omitted := omittedThinking(req, thinking)
	sign := s.signatureEnabled() && shouldSignThinking(req) && (thinking != "" || omitted)
	inTokens := EstimateAnthropicTokens(req)
	if inTokens < 1 {
		inTokens = 1
	}
	outTokens := estimateTokens(text) + estimateTokens(thinking)
	if outTokens < 1 {
		outTokens = 1
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
	usage := withThinkingUsage(composeUsage(inTokens, outTokens, cacheUsage, 0), req, thinking, text)
	msgID := emulation.MessageID()
	if entry != nil {
		entry.Model = req.Model
		entry.Prompt = usage.InputTokens
		entry.Completion = usage.OutputTokens
		entry.Total = usage.InputTokens + usage.OutputTokens
		entry.Latency = time.Since(start).Milliseconds()
	}
	countThinking := thinkingTokenCountRequested(r, req)
	if req.Stream {
		writeAnthropicSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		sw := NewSSEAnthropicWriter(w)
		_ = sw.Emit("message_start", officialMessageStart(msgID, req.Model, usage))
		_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
		idx := emitThinkingSignatureSSE(sw, 0, thinking, req.Model, msgID, countThinking, sign, omitted)
		if text == "" {
			text = "I don't know."
		}
		_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
			Type: "content_block_start", Index: idx,
			ContentBlock: AnthropicContentBlock{Type: "text", Text: ""},
		})
		_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
			Type: "content_block_delta", Index: idx,
			Delta: AnthropicBlockDelta{Type: "text_delta", Text: text},
		})
		_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: idx})
		_ = sw.Emit("message_delta", officialMessageDelta("end_turn", usage))
		_ = sw.Emit("message_stop", AnthropicEventMessageStop{Type: "message_stop"})
		return
	}
	var blocks []AnthropicContentBlock
	if thinking != "" || omitted {
		sig := ""
		if sign {
			sig = emulation.ThinkingSignature(thinking, req.Model, msgID)
		}
		blocks = append(blocks, AnthropicContentBlock{Type: "thinking", Thinking: thinking, Signature: sig})
	}
	if text == "" {
		text = "I don't know."
	}
	blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
	stop := "end_turn"
	writeAnthropicJSON(w, http.StatusOK, AnthropicMessageResponse{
		ID: msgID, Type: "message", Role: "assistant", Model: req.Model,
		Content: blocks, StopReason: &stop, Usage: usage,
	})
}

func (s *Server) signatureEnabled() bool {
	return s == nil || s.emu == nil || s.emu.SignatureEnabled()
}

func emitThinkingSignatureSSE(sw *SSEAnthropicWriter, index int, thinking, model, msgID string, countTokens, sign, omitted bool) int {
	if thinking == "" && !omitted {
		return index
	}
	_ = sw.Emit("content_block_start", AnthropicEventContentBlockStart{
		Type: "content_block_start", Index: index,
		ContentBlock: AnthropicContentBlock{Type: "thinking", Thinking: "", Signature: ""},
	})
	if thinking != "" {
		_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
			Type: "content_block_delta", Index: index,
			Delta: thinkingDelta(thinking, countTokens),
		})
	}
	if sign {
		if sig := emulation.ThinkingSignature(thinking, model, msgID); sig != "" {
			_ = sw.Emit("content_block_delta", AnthropicEventContentBlockDelta{
				Type: "content_block_delta", Index: index,
				Delta: AnthropicBlockDelta{Type: "signature_delta", Signature: sig},
			})
		}
	}
	_ = sw.Emit("content_block_stop", AnthropicEventContentBlockStop{Type: "content_block_stop", Index: index})
	return index + 1
}
