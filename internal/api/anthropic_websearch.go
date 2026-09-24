package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/admin"
	"prism-2api/internal/emulation"
	"prism-2api/internal/emulation/websearch"
)

func (s *Server) shouldInterceptWebSearch(raw []byte) bool {
	return s != nil && s.emu != nil && s.emu.WebSearchEnabled() && websearch.IsOnlyWebSearch(raw)
}

func (s *Server) handleEmulatedWebSearch(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, raw []byte, entry *admin.LogEntry) {
	start := time.Now()
	query := websearch.ExtractQuery(raw)
	if query == "" {
		query = lastUserText(req)
	}
	searchQuery := websearch.NormalizeSearchQuery(query)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	inTokens := EstimateAnthropicTokens(req)
	if inTokens < 1 {
		inTokens = 1
	}
	plan := s.cachePlan(r, raw, req.Model, inTokens)
	var cacheUsage *emulation.Usage
	if plan != nil {
		cacheUsage = plan.Result()
	}
	msgID := emulation.MessageID()
	startUsage := composeUsage(inTokens, 0, cacheUsage, 1)

	var sw *SSEAnthropicWriter
	if req.Stream {
		writeAnthropicSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		sw = NewSSEAnthropicWriter(w)
		_ = sw.Emit("message_start", officialMessageStart(msgID, req.Model, startUsage))
		_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
	}

	resp, provider, err := s.searchWeb(ctx, searchQuery)
	latency := time.Since(start).Milliseconds()
	if s.emu != nil {
		logEntry := emulation.SearchLog{Query: truncateRunes(query, 160), Provider: provider, LatencyMS: latency}
		if err != nil {
			logEntry.Error = err.Error()
		} else if resp != nil {
			logEntry.Results = len(resp.Results)
		}
		s.emu.NoteSearch(logEntry)
	}
	if err != nil {
		log.Printf("[websearch] search failed provider=%s: %v", provider, err)
		resp = &websearch.SearchResponse{Provider: provider}
	}

	text := websearch.TextSummary(searchQuery, resp)
	outTokens := estimateTokens(text)
	if outTokens < 1 {
		outTokens = 1
	}
	if plan != nil {
		plan.Commit()
		if s.emu != nil {
			s.emu.NoteCache(req.Model, inTokens, cacheUsage)
		}
	}
	usage := composeUsage(inTokens, outTokens, cacheUsage, 1)

	if entry != nil {
		entry.Model = req.Model
		entry.Prompt = usage.InputTokens
		entry.Completion = usage.OutputTokens
		entry.Total = usage.InputTokens + usage.OutputTokens
		entry.Latency = latency
	}

	if req.Stream {
		emitWebSearchSSE(sw, searchQuery, resp)
		_ = sw.Emit("message_delta", officialMessageDelta("end_turn", usage))
		_ = sw.Emit("message_stop", AnthropicEventMessageStop{Type: "message_stop"})
		return
	}

	blocks := webSearchContentBlocks(searchQuery, resp)
	stop := "end_turn"
	writeAnthropicJSON(w, http.StatusOK, AnthropicMessageResponse{
		ID: msgID, Type: "message", Role: "assistant", Model: req.Model,
		Content: blocks, StopReason: &stop, Usage: usage,
	})
}

func (s *Server) searchWeb(ctx context.Context, query string) (*websearch.SearchResponse, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, "", fmt.Errorf("web search: empty query")
	}
	cfg := emulation.DefaultConfig().WebSearch
	if s.emu != nil {
		cfg = s.emu.Config().WebSearch
	}
	max := cfg.MaxResults
	switch cfg.Provider {
	case emulation.ProviderTavily:
		resp, err := s.searchTavily(ctx, query, max)
		return resp, "tavily", err
	case emulation.ProviderVendor:
		resp, err := s.searchCursor(ctx, query, max)
		return resp, "vendor", err
	default:
		if resp, err := s.searchCursor(ctx, query, max); err == nil && resp != nil && len(resp.Results) > 0 {
			return resp, "vendor", nil
		} else if err != nil {
			log.Printf("[websearch] vendor provider failed, falling back to tavily: %v", err)
		}
		resp, err := s.searchTavily(ctx, query, max)
		return resp, "tavily", err
	}
}

func (s *Server) searchTavily(ctx context.Context, query string, max int) (*websearch.SearchResponse, error) {
	if s.emu == nil {
		return nil, fmt.Errorf("tavily: not configured")
	}
	return s.emu.SearchTavilyPool(ctx, query, max)
}

func (s *Server) searchCursor(ctx context.Context, query string, max int) (*websearch.SearchResponse, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("cursor websearch: no account pool")
	}
	acc, err := s.pickAny()
	if err != nil {
		return nil, fmt.Errorf("cursor websearch: %w", err)
	}
	prompt := fmt.Sprintf("Use WebSearch to search the public web for: %s\n\nReply with a markdown list only, one result per line:\n- [title](https://example.com): snippet\nNo preamble.", query)
	content, _ := json.Marshal(prompt)
	model := ""
	if s.cfg != nil {
		model = s.cfg.DefaultModel
	}
	if model == "" {
		model = "claude-4.5-sonnet"
	}
	chatReq := &ChatCompletionRequest{
		Model:    model,
		Messages: []ChatMessage{{Role: "user", Content: content}},
	}
	agentReq, err := MapChat(ctx, chatReq, s.promptText())
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	if err := acc.Client.Stream(ctx, agentReq, func(ev adapter.Event) bool {
		if ev.Text != "" {
			text.WriteString(ev.Text)
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("cursor websearch: %w", err)
	}
	results := websearch.ParseMarkdownResults(text.String(), max)
	if len(results) == 0 {
		return nil, fmt.Errorf("cursor websearch: no results")
	}
	return &websearch.SearchResponse{Results: results, Provider: "cursor"}, nil
}

func (s *Server) handleEmulatedCodeExecution(w http.ResponseWriter, r *http.Request, req *AnthropicMessagesRequest, raw []byte, entry *admin.LogEntry) {
	start := time.Now()
	query := lastUserText(req)
	stdout, _ := emulation.ExtractCodeExecution(query)
	text := strings.TrimSpace(stdout)
	inTokens := EstimateAnthropicTokens(req)
	if inTokens < 1 {
		inTokens = 1
	}
	outTokens := estimateTokens(text)
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
	usage := composeUsage(inTokens, outTokens, cacheUsage, 0)
	msgID := emulation.MessageID()
	container := &AnthropicContainer{ID: emulation.ContainerID()}
	if entry != nil {
		entry.Model = req.Model
		entry.Prompt = usage.InputTokens
		entry.Completion = outTokens
		entry.Total = usage.InputTokens + outTokens
		entry.Latency = time.Since(start).Milliseconds()
	}
	if req.Stream {
		writeAnthropicSSEHeaders(w)
		w.WriteHeader(http.StatusOK)
		sw := NewSSEAnthropicWriter(w)
		startEv := officialMessageStart(msgID, req.Model, usage)
		startEv.Message.Container = container
		_ = sw.Emit("message_start", startEv)
		_ = sw.Emit("ping", AnthropicEventPing{Type: "ping"})
		emitCodeExecutionSSE(sw, query)
		_ = sw.Emit("message_delta", officialMessageDelta("end_turn", usage))
		_ = sw.Emit("message_stop", AnthropicEventMessageStop{Type: "message_stop"})
		return
	}
	stop := "end_turn"
	writeAnthropicJSON(w, http.StatusOK, AnthropicMessageResponse{
		ID: msgID, Type: "message", Role: "assistant", Model: req.Model,
		Content: codeExecutionBlocks(query), StopReason: &stop, Usage: usage,
		Container: container,
	})
}

func lastUserText(req *AnthropicMessagesRequest) string {
	texts := lastUserTexts(req)
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func allUserText(req *AnthropicMessagesRequest) string {
	return strings.TrimSpace(strings.Join(lastUserTexts(req), "\n"))
}

func lastUserTexts(req *AnthropicMessagesRequest) []string {
	if req == nil {
		return nil
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil && strings.TrimSpace(plain) != "" {
			return []string{strings.TrimSpace(plain)}
		}
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil
		}
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				texts = append(texts, strings.TrimSpace(b.Text))
			}
		}
		return texts
	}
	return nil
}

func truncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	i := 0
	for idx := range s {
		if i == n {
			return s[:idx]
		}
		i++
	}
	return s
}
