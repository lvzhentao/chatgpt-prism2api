package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Message Batches API（对齐官方 /v1/messages/batches） ----------
//
// 官方端点：
//
//	POST   /v1/messages/batches               创建批次（异步，202 + batch 对象）
//	GET    /v1/messages/batches               列出批次
//	GET    /v1/messages/batches/{batch_id}    查询状态
//	POST   /v1/messages/batches/{batch_id}/cancel  取消
//	GET    /v1/messages/batches/{batch_id}/results 下载结果（JSONL）
//
// 实现：内存存储 + 后台 goroutine 逐条转发到 Cursor（复用 /v1/messages 同链路）。
// 官方批次的 50% 折扣是官方侧定价，代理侧逐条转发语义等价。

// batch 是内存中的批次记录。
type batch struct {
	id               string
	requests         []batchRequestItem
	results          []batchResultItem
	processingStatus string // in_progress | ended | canceling | cancelled
	createdAt        time.Time
	expiresAt        time.Time
	endedAt          *time.Time
	cancelInitiated  *time.Time
}

// batchRequestItem 是批次中的单条请求。
type batchRequestItem struct {
	CustomID string                    `json:"custom_id"`
	Params   *AnthropicMessagesRequest `json:"params"`
}

// batchResultItem 是单条结果（写入 results JSONL）。
type batchResultItem struct {
	CustomID string `json:"custom_id"`
	Result   any    `json:"result"`
}

// batchSucceededResult 对应官方 succeeded 结果。
type batchSucceededResult struct {
	Type    string                    `json:"type"` // "succeeded"
	Message *AnthropicMessageResponse `json:"message"`
}

// batchErroredResult 对应官方 errored 结果。
type batchErroredResult struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type batchPersister interface {
	UpsertBatch(id, status string, payload []byte) error
	ListBatchPayloads() ([][]byte, error)
}

// batchStore 是批次内存存储，可选落 PostgreSQL。
type batchStore struct {
	mu      sync.Mutex
	batches map[string]*batch
	order   []string // 创建顺序（列表用）
	persist batchPersister
}

func newBatchStore() *batchStore {
	return &batchStore{batches: make(map[string]*batch)}
}

func (bs *batchStore) setPersist(p batchPersister) {
	if bs == nil {
		return
	}
	bs.mu.Lock()
	bs.persist = p
	bs.mu.Unlock()
}

func (bs *batchStore) save(b *batch) {
	if bs == nil || b == nil {
		return
	}
	bs.mu.Lock()
	p := bs.persist
	bs.mu.Unlock()
	if p == nil {
		return
	}
	payload, err := json.Marshal(batchToDTO(b))
	if err != nil {
		return
	}
	_ = p.UpsertBatch(b.id, b.processingStatus, payload)
}

// newBatchID 生成批次 ID（官方前缀 msgbatch_）。
func newBatchID() string {
	return "msgbatch_" + randomHex(16)
}

// createBatch 创建批次并启动后台处理。
func (bs *batchStore) createBatch(items []batchRequestItem, ttl time.Duration) *batch {
	now := time.Now()
	b := &batch{
		id:               newBatchID(),
		requests:         items,
		processingStatus: "in_progress",
		createdAt:        now,
		expiresAt:        now.Add(ttl),
	}
	bs.mu.Lock()
	bs.batches[b.id] = b
	bs.order = append(bs.order, b.id)
	bs.mu.Unlock()
	bs.save(b)
	return b
}

// get 返回批次（nil 表示不存在）。
func (bs *batchStore) get(id string) *batch {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.batches[id]
}

// appendResult 追加单条结果。
func (bs *batchStore) appendResult(b *batch, customID string, res any) {
	bs.mu.Lock()
	b.results = append(b.results, batchResultItem{CustomID: customID, Result: res})
	bs.mu.Unlock()
	bs.save(b)
}

// isCancelled 检查批次是否被取消；取消时标记终态。
func (bs *batchStore) isCancelled(b *batch) bool {
	bs.mu.Lock()
	if b.processingStatus == "canceling" || b.processingStatus == "cancelled" {
		changed := false
		if b.processingStatus == "canceling" {
			b.processingStatus = "cancelled"
			b.endedAt = nowPtr()
			changed = true
		}
		bs.mu.Unlock()
		if changed {
			bs.save(b)
		}
		return true
	}
	bs.mu.Unlock()
	return false
}

// finish 结束批次（进行中 → ended）。
func (bs *batchStore) finish(b *batch) {
	bs.mu.Lock()
	if b.processingStatus == "in_progress" {
		b.processingStatus = "ended"
		b.endedAt = nowPtr()
	}
	bs.mu.Unlock()
	bs.save(b)
}

// list 按创建时间倒序返回批次快照。
func (bs *batchStore) list() []*batch {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	out := make([]*batch, 0, len(bs.order))
	for i := len(bs.order) - 1; i >= 0; i-- {
		if b := bs.batches[bs.order[i]]; b != nil {
			out = append(out, b)
		}
	}
	return out
}

// cancel 标记取消（进行中的请求不会被中断，完成后标记 canceled 或保留结果）。
func (bs *batchStore) cancel(id string) bool {
	bs.mu.Lock()
	b := bs.batches[id]
	if b == nil || b.processingStatus != "in_progress" {
		bs.mu.Unlock()
		return false
	}
	b.processingStatus = "canceling"
	now := time.Now()
	b.cancelInitiated = &now
	bs.mu.Unlock()
	bs.save(b)
	return true
}

// processBatch 后台逐条转发请求。
func (s *Server) processBatch(b *batch) {
	for _, item := range b.requests {
		if s.batchStore.isCancelled(b) {
			return
		}
		if item.Params == nil {
			s.batchStore.appendResult(b, item.CustomID, batchErroredResult{Type: "errored"})
			continue
		}
		resp, err := s.doAnthropicMessageOnce(item.Params)
		if err != nil {
			res := batchErroredResult{Type: "errored"}
			res.Error.Type = "api_error"
			res.Error.Message = err.Error()
			s.batchStore.appendResult(b, item.CustomID, res)
			continue
		}
		s.batchStore.appendResult(b, item.CustomID, batchSucceededResult{Type: "succeeded", Message: resp})
	}
	s.batchStore.finish(b)
}

func nowPtr() *time.Time {
	t := time.Now()
	return &t
}

// batchResponse 是官方 batch 对象（HTTP 响应）。
type batchResponse struct {
	ID                string      `json:"id"`
	Type              string      `json:"type"` // "message_batch"
	ProcessingStatus  string      `json:"processing_status"`
	RequestCounts     batchCounts `json:"request_counts"`
	EndedAt           *time.Time  `json:"ended_at"`
	CreatedAt         time.Time   `json:"created_at"`
	ExpiresAt         time.Time   `json:"expires_at"`
	CancelInitiatedAt *time.Time  `json:"cancel_initiated_at"`
	ResultsURL        string      `json:"results_url,omitempty"`
}

// batchCounts 是请求计数。
type batchCounts struct {
	Processing int `json:"processing"`
	Succeeded  int `json:"succeeded"`
	Errored    int `json:"errored"`
	Canceled   int `json:"canceled"`
	Expired    int `json:"expired"`
}

// batchToResponse 构造官方格式响应。
func batchToResponse(b *batch, resultsURL string) batchResponse {
	counts := batchCounts{Processing: len(b.requests)}
	for _, r := range b.results {
		if resultKind(r.Result) == "succeeded" {
			counts.Succeeded++
		} else {
			counts.Errored++
		}
	}
	counts.Processing = len(b.requests) - counts.Succeeded - counts.Errored
	if b.processingStatus == "cancelled" {
		counts.Canceled = counts.Processing
		counts.Processing = 0
	}
	return batchResponse{
		ID:                b.id,
		Type:              "message_batch",
		ProcessingStatus:  b.processingStatus,
		RequestCounts:     counts,
		EndedAt:           b.endedAt,
		CreatedAt:         b.createdAt,
		ExpiresAt:         b.expiresAt,
		CancelInitiatedAt: b.cancelInitiated,
		ResultsURL:        resultsURL,
	}
}

// handleBatchesCreate 处理 POST /v1/messages/batches。
func (s *Server) handleBatchesCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]string{"type": "method_not_allowed_error", "message": "method not allowed"},
		})
		return
	}
	var req struct {
		Requests []batchRequestItem `json:"requests"`
	}
	if err := json.NewDecoder(ioLimit(r.Body)).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	if len(req.Requests) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "requests must not be empty")
		return
	}
	// 校验每条请求（custom_id 必填，params 为 MessagesRequest）
	for _, item := range req.Requests {
		if item.CustomID == "" {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "each request requires custom_id")
			return
		}
		if item.Params == nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "each request requires params")
			return
		}
	}
	b := s.batchStore.createBatch(req.Requests, 24*time.Hour)
	go s.processBatch(b)
	writeJSON(w, http.StatusAccepted, batchToResponse(b, ""))
}

// handleBatchesList 处理 GET /v1/messages/batches。
func (s *Server) handleBatchesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	batches := s.batchStore.list()
	out := make([]batchResponse, 0, len(batches))
	for _, b := range batches {
		out = append(out, batchToResponse(b, resultsURL(r, b.id)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "has_more": false})
}

// handleBatchesGet 处理 GET /v1/messages/batches/{id}。
func (s *Server) handleBatchesGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	id := batchIDFromPath(r.URL.Path)
	b := s.batchStore.get(id)
	if b == nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "batch not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, batchToResponse(b, resultsURL(r, b.id)))
}

// handleBatchesCancel 处理 POST /v1/messages/batches/{id}/cancel。
func (s *Server) handleBatchesCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	id := batchIDFromPath(r.URL.Path)
	if !s.batchStore.cancel(id) {
		b := s.batchStore.get(id)
		if b == nil {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error", "batch not found: "+id)
			return
		}
		// 已结束/已取消：官方返回当前状态
		writeJSON(w, http.StatusOK, batchToResponse(b, resultsURL(r, b.id)))
		return
	}
	b := s.batchStore.get(id)
	writeJSON(w, http.StatusOK, batchToResponse(b, resultsURL(r, b.id)))
}

// handleBatchesResults 处理 GET /v1/messages/batches/{id}/results（JSONL）。
func (s *Server) handleBatchesResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]string{"type": "method_not_allowed_error"}})
		return
	}
	id := batchIDFromPath(r.URL.Path)
	b := s.batchStore.get(id)
	if b == nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "batch not found: "+id)
		return
	}
	if b.processingStatus != "ended" && b.processingStatus != "cancelled" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"batch is not yet complete: "+b.processingStatus)
		return
	}
	// 结果按 custom_id 排序（官方行为）
	sorted := make([]batchResultItem, len(b.results))
	copy(sorted, b.results)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CustomID < sorted[j].CustomID })
	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s_results.jsonl", b.id))
	for _, item := range sorted {
		line, _ := json.Marshal(item)
		w.Write(append(line, '\n'))
	}
}

// handleBatchesCreateList 分发 POST(创建)/GET(列表)。
func (s *Server) handleBatchesCreateList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleBatchesCreate(w, r)
		return
	}
	s.handleBatchesList(w, r)
}

// handleBatchesSubresource 分发批次子资源：{id} / {id}/cancel / {id}/results。
func (s *Server) handleBatchesSubresource(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/cancel"):
		s.handleBatchesCancel(w, r)
	case strings.HasSuffix(r.URL.Path, "/results"):
		s.handleBatchesResults(w, r)
	default:
		s.handleBatchesGet(w, r)
	}
}

// batchIDFromPath 从 /v1/messages/batches/{id}/... 路径提取批次 ID。
func batchIDFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// ["v1","messages","batches", id, ...]
	for i, p := range parts {
		if p == "batches" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// resultsURL 构造 results 下载地址。
func resultsURL(r *http.Request, id string) string {
	return "https://" + r.Host + "/v1/messages/batches/" + id + "/results"
}

// doAnthropicMessageOnce 单条消息请求（复用 handleChat 全链路：账号池/重试/模型映射）。
func (s *Server) doAnthropicMessageOnce(req *AnthropicMessagesRequest) (*AnthropicMessageResponse, error) {
	chatReq, err := AnthropicToOpenAIRequest(req, nil, "")
	if err != nil {
		return nil, err
	}
	chatReq.Stream = false
	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	s.handleChat(rec, internalChatRequest(nil, body))
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("batch item failed: %d %s", rec.Code, truncateStr(rec.Body.String(), 300))
	}
	var resp struct {
		Choices []struct {
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parse batch item response: %w", err)
	}
	msg := &AnthropicMessageResponse{
		ID:      "msg_" + randomHex(8),
		Type:    "message",
		Role:    "assistant",
		Content: make([]AnthropicContentBlock, 0),
		Model:   chatReq.Model,
	}
	if len(resp.Choices) > 0 {
		msg.Content = anthropicBlocksFromOpenAI(&resp.Choices[0].Message)
		sr := openAIStopToAnthropic(resp.Choices[0].FinishReason)
		msg.StopReason = &sr
	}
	if resp.Usage != nil {
		msg.Usage = AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}
	return msg, nil
}

type batchDTO struct {
	ID               string             `json:"id"`
	Requests         []batchRequestItem `json:"requests"`
	Results          []batchResultItem  `json:"results"`
	ProcessingStatus string             `json:"processing_status"`
	CreatedAt        time.Time          `json:"created_at"`
	ExpiresAt        time.Time          `json:"expires_at"`
	EndedAt          *time.Time         `json:"ended_at,omitempty"`
	CancelInitiated  *time.Time         `json:"cancel_initiated_at,omitempty"`
}

func batchToDTO(b *batch) batchDTO {
	return batchDTO{
		ID:               b.id,
		Requests:         b.requests,
		Results:          b.results,
		ProcessingStatus: b.processingStatus,
		CreatedAt:        b.createdAt,
		ExpiresAt:        b.expiresAt,
		EndedAt:          b.endedAt,
		CancelInitiated:  b.cancelInitiated,
	}
}

func resultKind(res any) string {
	switch v := res.(type) {
	case batchSucceededResult:
		return v.Type
	case batchErroredResult:
		return v.Type
	case map[string]any:
		if t, _ := v["type"].(string); t != "" {
			return t
		}
	}
	return "errored"
}

func (s *Server) restoreBatches() {
	if s == nil || s.pg == nil || s.batchStore == nil {
		return
	}
	payloads, err := s.pg.ListBatchPayloads()
	if err != nil {
		log.Printf("restore batches: %v", err)
		return
	}
	for _, raw := range payloads {
		var dto batchDTO
		if err := json.Unmarshal(raw, &dto); err != nil || dto.ID == "" {
			continue
		}
		b := &batch{
			id:               dto.ID,
			requests:         dto.Requests,
			results:          dto.Results,
			processingStatus: dto.ProcessingStatus,
			createdAt:        dto.CreatedAt,
			expiresAt:        dto.ExpiresAt,
			endedAt:          dto.EndedAt,
			cancelInitiated:  dto.CancelInitiated,
		}
		s.batchStore.mu.Lock()
		if _, ok := s.batchStore.batches[b.id]; !ok {
			s.batchStore.batches[b.id] = b
			s.batchStore.order = append(s.batchStore.order, b.id)
		}
		s.batchStore.mu.Unlock()
		if b.processingStatus == "in_progress" {
			go s.processBatch(b)
		}
	}
}

// randomHex 生成 n 字节随机 hex 字符串（批次/消息 ID）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// truncateStr 截断字符串（错误信息用）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// anthropicBlocksFromOpenAI 将 OpenAI 消息转为 Anthropic content blocks。
func anthropicBlocksFromOpenAI(m *ChatMessage) []AnthropicContentBlock {
	var out []AnthropicContentBlock
	if m == nil {
		return out
	}
	if m.Reasoning != "" {
		out = append(out, AnthropicContentBlock{Type: "thinking", Thinking: m.Reasoning})
	}
	if t := contentText(m.Content); t != "" {
		out = append(out, AnthropicContentBlock{Type: "text", Text: t})
	}
	for _, tc := range m.ToolCalls {
		out = append(out, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out
}

// openAIStopToAnthropic 映射 stop_reason。
func openAIStopToAnthropic(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		return "end_turn"
	}
}
