package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/adapter"
)

// ============================================================
// 定制点 4 — OpenAI 形状 → Prism 请求体，以及轮询结果 → adapter.Event。
// 上游没有 SSE：一轮对话 = start（提交）+ status（轮询），正文一次性到达。
// ============================================================

func mapChat(req adapter.ChatRequest) (*adapter.NativeRequest, error) {
	nr := &adapter.NativeRequest{
		Model:        req.Model,
		Messages:     req.Messages,
		Tools:        req.Tools,
		Stream:       req.Stream,
		Temperature:  req.Temperature,
		MaxTokens:    req.MaxTokens,
		Thinking:     req.ReasoningEffort,
		SystemPrompt: req.SystemPrompt,
		Extra:        map[string]any{},
	}
	return nr, nil
}

// buildInput 把整段对话摊平成 Prism 的 input 数组。
// 真机验证：上游只把「最后一条 user 消息」当作本轮请求，其余 system 消息进 Context；
// assistant/user 历史消息不会自动带上，必须折叠成 Context 文本，否则模型看不到上下文。
//
// 附件（图片/文档）在这里变成文本块（上游没有二进制通道，见 attachments.go）；
// 内联预算按「新消息优先」分配——历史里的图片在预算用尽时只留标记，不再重复内联。
func buildInput(nr *adapter.NativeRequest) []any {
	out := make([]any, 0, len(nr.Messages)+2)
	sysTexts := make([]string, 0, 2)
	transcript := make([]string, 0, len(nr.Messages))
	toolResults := make([]string, 0, 4)
	lastUser := ""
	lastUserIdx := -1
	var lastUserFiles []adapter.File
	for i := len(nr.Messages) - 1; i >= 0; i-- {
		m := nr.Messages[i]
		// 图片-only 的请求没有文本，但同样是本轮请求（漏判会把附件当历史丢掉）。
		if isUserRole(m.Role) && (strings.TrimSpace(m.Content) != "" || len(m.Files) > 0) {
			lastUserIdx = i
			lastUser = strings.TrimSpace(m.Content)
			lastUserFiles = m.Files
			break
		}
	}

	// 附件预算：新消息优先（本轮 user 先分，再往历史里回填）。
	budget := inlineB64Total
	lastUserBlocks := attachmentBlocks(lastUserFiles, &budget)
	historyBlocks := make(map[int][]string, 2)
	for i := lastUserIdx - 1; i >= 0; i-- {
		if len(nr.Messages[i].Files) > 0 && budget > 0 {
			historyBlocks[i] = attachmentBlocks(nr.Messages[i].Files, &budget)
		}
	}

	// 工具结果归属：优先按 tool_call_id 匹配；客户端没带 id 时按出现顺序认领。
	pending := make([]adapter.ToolCall, 0, 4)
	for _, m := range nr.Messages {
		for _, tc := range m.ToolCalls {
			pending = append(pending, tc)
		}
	}
	claimToolName := func(id string) string {
		id = strings.TrimSpace(id)
		if id != "" {
			for i, tc := range pending {
				if tc.ID == id {
					pending = append(pending[:i], pending[i+1:]...)
					return tc.Name
				}
			}
		}
		if len(pending) > 0 {
			name := pending[0].Name
			pending = pending[1:]
			return name
		}
		return ""
	}

	for i, m := range nr.Messages {
		text := strings.TrimSpace(m.Content)
		switch {
		case isSystemRole(m.Role):
			if text != "" {
				sysTexts = append(sysTexts, text)
			}
		case i == lastUserIdx:
			// 本轮请求，最后再放
		case isAssistantRole(m.Role):
			if text != "" {
				transcript = append(transcript, "Assistant: "+text)
			}
			for _, tc := range m.ToolCalls {
				if tc.Name != "" {
					transcript = append(transcript, "Assistant[tool_call]: "+tc.Name+" "+strings.TrimSpace(tc.Arguments))
				}
			}
		case isToolRole(m.Role):
			label := "工具"
			if n := claimToolName(m.ToolCallID); n != "" {
				label = n
			}
			if text == "" {
				text = "(空)"
			}
			toolResults = append(toolResults, "- "+label+": "+text)
		default: // user / function
			line := text
			if blocks := historyBlocks[i]; len(blocks) > 0 {
				line = strings.TrimSpace(line + "\n" + strings.Join(blocks, "\n\n"))
			} else if len(m.Files) > 0 {
				line = strings.TrimSpace(line + " " + fileMarkers(m.Files))
			}
			if line == "" {
				line = "(empty)"
			}
			transcript = append(transcript, "User: "+line)
		}
	}

	// 带 tools 的请求：上游不再采信 system 内容。
	// 真机证据（2026-09-17）：同一句 "回答必须以 ZZZ 开头" 的 system 指令，不带 tools 时生效
	// （答 "ZZZ 2"），带上 tools 就被忽略（答 "2"）；system 里塞对话历史/工具结果同样不生效，
	// 而放进「最后一条 user 消息」则稳定生效（M8/M10/M11/M12 全绿）。
	if len(toolDefs(nr)) > 0 {
		blocks := make([]string, 0, 5)
		if injected := systemPromptFor(nr.SystemPrompt); injected != "" {
			blocks = append(blocks, "[系统指令]\n"+injected)
		}
		if len(sysTexts) > 0 {
			blocks = append(blocks, "[客户端指令]\n"+strings.Join(sysTexts, "\n\n"))
		}
		if len(transcript) > 0 {
			blocks = append(blocks, "[对话历史]\n"+strings.Join(transcript, "\n"))
		}
		// 已执行结果单独成块：混在 [对话历史] 里时模型会声称"看不到返回结果"。
		// 措辞要点：这是"客户端在它自己的机器上真实执行完、刚返回的数据"，不是可选项——
		// 多步链路里模型常无视它、回头翻自己的工作区，所以必须点名"不要回头检查、不要自己重做一遍"。
		if len(toolResults) > 0 {
			blocks = append(blocks, "[已执行工具的结果——客户端已在它自己的机器上真实执行，数据如下，直接用]\n"+strings.Join(toolResults, "\n")+"\n(不要回头用你自己的环境验证它，不要自己重做一遍，直接用上面的数据继续。)")
		}
		blocks = append(blocks, toolContract(nr))
		if lastUser != "" {
			blocks = append(blocks, "[用户当前消息]\n"+lastUser)
		}
		if len(lastUserBlocks) > 0 {
			blocks = append(blocks, lastUserBlocks...)
		}
		// 协议强化重试（同账号第二轮）：纠正块放最后，与用户消息相邻，遵守率最高。
		if rb := toolReinforceBlock(nr); rb != "" {
			blocks = append(blocks, rb)
		}
		// 交付追捞（同账号第二轮，与协议强化互斥）：假交付/翻环境拒绝的纠正块同样放最后。
		if db := deliverySalvageBlock(nr); db != "" {
			blocks = append(blocks, db)
		}
		return append(out, messageItem("user", strings.Join(blocks, "\n\n")))
	}

	// 不带 tools 的请求：所有前文合并成**一条** system item（与 tools 分支同构）。
	// 多轮真机证据（2026-09-17，ENHANCE-PLAN P0-1 的 V2/M1/C2m）：多条 system 并存时
	// 最后一条盖掉前面的——[对话历史] 恒在最后，会把客户端 system 整条吃掉；
	// 合并成一条后不依赖任何顺序假设。
	blocks := make([]string, 0, 4)
	if injected := systemPromptFor(nr.SystemPrompt); injected != "" {
		blocks = append(blocks, injected)
	}
	if len(sysTexts) > 0 {
		blocks = append(blocks, "[客户端指令]\n"+strings.Join(sysTexts, "\n\n"))
	}
	if len(transcript) > 0 {
		blocks = append(blocks, "[对话历史]\n"+strings.Join(transcript, "\n"))
	}
	if len(toolResults) > 0 {
		blocks = append(blocks, "[已执行工具的结果]\n"+strings.Join(toolResults, "\n"))
	}
	if len(blocks) > 0 {
		out = append(out, messageItem("system", strings.Join(blocks, "\n\n")))
	}
	if lastUser != "" || len(lastUserBlocks) > 0 {
		// 交付追捞（无 tools 的纯聊天也会触发，见 deliverySalvageBlock）：纠正块拼在
		// user 消息末尾——无 tools 分支里这是唯一与"本轮请求"相邻的位置，遵守率最高。
		msg := joinText(lastUser, lastUserBlocks)
		if db := deliverySalvageBlock(nr); db != "" {
			msg = strings.TrimSpace(strings.TrimRight(msg, "\n") + "\n\n" + db)
		}
		out = append(out, messageItem("user", msg))
	}
	if len(out) == 0 {
		out = append(out, messageItem("user", "(empty)"))
	}
	return out
}

// joinText 把用户文本与附件块拼成一条消息正文（附件块在后）。
func joinText(text string, blocks []string) string {
	parts := make([]string, 0, len(blocks)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, strings.TrimSpace(text))
	}
	parts = append(parts, blocks...)
	return strings.Join(parts, "\n\n")
}

// fileMarkers 把没有内联到位的附件折成一行标记（历史里的图片超预算时用）。
func fileMarkers(files []adapter.File) string {
	names := make([]string, 0, len(files))
	for i, f := range files {
		names = append(names, attachmentName(f.Name, f.Mime, i))
	}
	if len(names) == 0 {
		return ""
	}
	return "[此前发送的附件（本轮未载入）：" + strings.Join(names, ", ") + "]"
}

func messageItem(role, text string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func isSystemRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return true
	}
	return false
}

func isUserRole(role string) bool {
	return strings.ToLower(strings.TrimSpace(role)) == "user"
}

func isAssistantRole(role string) bool {
	return strings.ToLower(strings.TrimSpace(role)) == "assistant"
}

func isToolRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "tool", "function":
		return true
	}
	return false
}

// splitModelSuffix 拆出裸模型名与等级后缀（-low/-medium/-high/-xhigh/-max）。
// 内核只在客户端没显式给 effort 时才还原裸名（api.applyOpenAIThinkingDefaults 在
// ReasoningEffort != "" 时直接返回），Anthropic 入口更是完全没走这一步；
// 带后缀的名字发到 Prism 一律被判 400，所以在 adapter 里兜底。
func splitModelSuffix(model string) (string, string) {
	if base, level, _ := adapter.ParseModelID(model); level != "" {
		return base, level
	}
	return model, ""
}

// modelCatalog 是进程内的静态模型表（Resolve 只查内存切片，不发请求），建一次即可。
var modelCatalog = sync.OnceValue(func() adapter.Catalog {
	return adapter.NewStaticCatalog(staticModels())
})

// modelName 把客户端给的模型名换成上游认的完整 ID。
// 上游只认清单里的完整 ID：真机实测 'gpt-5.6-sol' 200，而别名（'sol'/'default'/'free'/
// 'astra'/'prism'/'chatgpt'）和带思考后缀的名字（'gpt-5.6-sol-high'）会原样发上去、
// 被判「Error while processing conversation (400 Bad Request)」，所以这里查一次目录，
// 命中别名换成 ServerModelName，顺带把思考后缀拆掉（档位由 reasoningEffort 另行还原）。
func modelName(nr *adapter.NativeRequest) string {
	m := strings.TrimSpace(nr.Model)
	if m == "" {
		return DefaultModel
	}
	// Resolve 对目录外名字返回原文（无 miss 标志），这里显式扫目录：
	// 命中 ID/别名（含去思考后缀的裸名）换 ServerModelName。
	base, _, _ := adapter.ParseModelID(m)
	for _, mi := range staticModels() {
		hit := mi.ID == m || mi.ID == base
		if !hit {
			for _, a := range mi.Aliases {
				if a == m || a == base {
					hit = true
					break
				}
			}
		}
		if hit {
			if sm := strings.TrimSpace(mi.ServerModelName); sm != "" {
				return sm
			}
			return mi.ID
		}
	}
	// 目录外的名字（如客户端发裸 'gpt-5'）原样上去就是 400（2026-09-19 线上
	// 实锤：403 时代被 sentinel 掩盖，sentinel 修复后立刻显形），兜底到默认模型。
	return DefaultModel
}

// reasoningEffort 把内核的 thinking 档位映射到 metadata.reasoning_effort。
// 客户端没给档位时，用模型名后缀兜底（gpt-5.6-sol-high 等价于 high）。
// xhigh 是实验档：放开侧直接透传给上游，不再静默夹成 high；max 暂归一到 xhigh 发（上游是否收 max 另说）。
// TODO(Main 生产实测)：若上游 400 拒收 xhigh，回退时把下面 xhigh/max 两支 case 改回 return "high" 即可。
func reasoningEffort(nr *adapter.NativeRequest) string {
	effort := strings.TrimSpace(nr.Thinking)
	if effort == "" {
		_, effort = splitModelSuffix(strings.TrimSpace(nr.Model))
	}
	switch strings.ToLower(effort) {
	case "low", "minimal":
		return "low"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "xhigh"
	default:
		return "medium"
	}
}

// listenSnapshot 是服务端要求的会话快照（首轮：codex_session_id/last_turn_id 为空）。
func listenSnapshot(userID, projectID, conv, sandboxURL, sandboxToken string) string {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	snap := map[string]any{
		"user_id":              userID,
		"project_id":           projectID,
		"conversation_id":      conv,
		"sandbox_url":          sandboxURL,
		"sandbox_token":        sandboxToken,
		"workspace_session_id": strings.TrimPrefix(conv, "cdx1_"),
		"codex_session_id":     nil,
		"last_turn_id":         nil,
		"endpoint_identity":    nil,
		"last_exec_at":         nil,
		"transcript_cursor":    0,
		"created_at":           now,
		"updated_at":           now,
		"last_saved_at":        nil,
	}
	raw, _ := json.Marshal(snap)
	return string(raw)
}

// Stream 执行一轮对话：会话自举 → 预热沙箱 → start → 轮询 → 一次性吐出正文。
// 浏览器代发通道（终局形态，见 relay.go）优先：上游全 API 面强验证后直连
// 通道只在 relay 关闭/降级时使用。
func (c *httpClient) Stream(ctx context.Context, nr *adapter.NativeRequest, emit func(adapter.Event) bool) error {
	if browserRelayOn() {
		return c.relayStream(ctx, nr, emit)
	}
	tAll := time.Now()
	t := time.Now()
	if _, err := c.loadCredential(); err != nil {
		return err
	}
	credMs := time.Since(t).Milliseconds()
	// T0.1：hit 标记在 ensure 前采样（缓存命中=热路径，0 RTT）。
	sessionHit := c.sessionValid()
	t = time.Now()
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	sessionMs := time.Since(t).Milliseconds()
	c.mu.Lock()
	projectHit := c.projectID != ""
	c.mu.Unlock()
	t = time.Now()
	projectID, err := c.ensureProject(ctx)
	if err != nil {
		return err
	}
	projectMs := time.Since(t).Milliseconds()
	sandboxHit := c.sandboxToken() != ""

	start, err := c.startTurn(ctx, nr, projectID)
	if err != nil {
		return err
	}
	res, err := c.pollTurn(ctx, start, emit)
	if err != nil {
		return err
	}
	// T0.1：收尾汇总一行（sandbox/register/start 由 startTurn 内累计，poll 由 pollTurn 内累计）。
	// P1-3 观测前置：upstream_calls=start_calls+poll_calls（每次 start/status 上游 HTTP 调用都计数，
	// 供后续观测判断上游负载；start_calls 含换沙箱重试与 reconnect 重试，poll_calls=Polls）。
	log.Printf("prism: turn cred_ms=%d session_ms=%d project_ms=%d sandbox_ms=%d register_ms=%d start_ms=%d poll_ms=%d polls=%d sandbox_hit=%t session_hit=%t project_hit=%t start_reused=%t total_ms=%d upstream_calls=%d start_calls=%d poll_calls=%d",
		credMs, sessionMs, projectMs, start.SandboxMs, start.RegisterMs, start.StartMs,
		res.PollMs, res.Polls, sandboxHit, sessionHit, projectHit, start.StartReused, time.Since(tAll).Milliseconds(),
		start.StartCalls+res.Polls, start.StartCalls, res.Polls)
	if res.Err != "" {
		return fmt.Errorf("prism: turn failed: %s", res.Err)
	}
	// 上游没有原生工具通道：正文里的 <tool_call> 块由适配器解析成内核的 ToolCall 事件。
	text, calls := res.Text, res.ToolCalls
	if len(nr.Tools) > 0 {
		if emulated, cleaned := extractToolCallsFromText(res.Text); len(emulated) > 0 {
			calls = append(calls, emulated...)
			text = cleaned
		}
	}
	// 上游 reasoning item 的思考摘要：在正文之前作为 Thinking 事件透传给客户端
	// （api 层映射成 reasoning_content / reasoning_summary 帧，并计入 completion_tokens）。
	if res.Reasoning != "" {
		if !emit(adapter.Event{Thinking: &adapter.Thinking{Text: res.Reasoning, IsLastThinkingChunk: true}}) {
			return nil
		}
	}
	if len(nr.Tools) > 0 || len(calls) > 0 {
		log.Printf("prism: turn tools_sent=%d tool_calls=%d text_len=%d item_types=%s",
			len(nr.Tools), len(calls), len(text), res.ItemTypes)
	}
	if text != "" && !emit(adapter.Event{Text: text}) {
		return nil
	}
	for i := range calls {
		tc := calls[i]
		if !emit(adapter.Event{ToolCall: &tc}) {
			return nil
		}
	}
	emit(adapter.Event{Ended: true})
	return nil
}

// registerConversation 让后端登记会话 id（best-effort：失败不阻断对话）。
// T1.2：调用带独立 5s 超时，与请求 ctx 解耦（context.WithoutCancel）：上游 hang 时
// fire-and-forget 的热路径 goroutine 不泄漏，冷路径的并发 register 不拖住 start。
func (c *httpClient) registerConversation(ctx context.Context, projectID, conv string) error {
	regCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registerTimeout)
	defer cancel()
	resp, err := c.request(regCtx, "POST", c.origin()+PathConversationHistory, map[string]any{
		"conversationId": conv,
		"order":          "desc",
		"limit":          50,
		"userId":         c.accountID(),
		"projectId":      projectID,
	}, nil)
	if err != nil {
		return nil
	}
	if resp.status == 401 || resp.status == 403 {
		return &upstreamError{Status: resp.status, Msg: "conversation-history: " + truncate(string(resp.body), 200)}
	}
	return nil
}

// ListModels 上游没有模型目录接口，返回 ErrNotImplemented，目录走静态表。
func (c *httpClient) ListModels(ctx context.Context) ([]adapter.ModelInfo, error) {
	return nil, adapter.ErrNotImplemented
}

// UploadImage 站点走项目文件管理，不做 data: URL 内联上传。
func (c *httpClient) UploadImage(ctx context.Context, filename, mime string, data []byte) (string, error) {
	return "", adapter.ErrNotImplemented
}

// FetchUsage 用量页数据：entitlements(planType/workspace) + session(email/user)。
// 上游没有额度百分比，所以只填身份与套餐字段（不产生自动禁用）。
func (c *httpClient) FetchUsage(ctx context.Context, websiteURL string) (adapter.UsageSnapshot, error) {
	if _, err := c.loadCredential(); err != nil {
		return adapter.UsageSnapshot{}, err
	}
	if err := c.ensureSession(ctx); err != nil {
		return adapter.UsageSnapshot{}, err
	}
	snap := adapter.UsageSnapshot{FetchedAt: time.Now().Unix()}

	resp, err := c.request(ctx, "GET", c.origin()+PathSession, nil, nil)
	if err != nil {
		return snap, err
	}
	if resp.status >= 300 {
		return snap, &upstreamError{Status: resp.status, Msg: "auth/session: " + truncate(string(resp.body), 200)}
	}
	v := resp.json()
	c.absorbAccount(v)
	user := mapAt(v, "user")
	snap.Email = stringAt(user, "email")
	snap.WorkOSID = stringAt(user, "id")

	resp, err = c.request(ctx, "GET", c.origin()+PathEntitlements, nil, nil)
	if err != nil {
		return snap, err
	}
	if resp.status >= 300 {
		return snap, &upstreamError{Status: resp.status, Msg: "entitlements: " + truncate(string(resp.body), 200)}
	}
	ent := resp.json()
	snap.PlanLabel = stringAt(ent, "planType")
	snap.IndividualPlan = stringAt(ent, "planType")
	snap.MembershipType = stringAt(ent, "businessMembershipType")
	snap.TeamMembershipType = stringAt(ent, "workspaceType")
	snap.SubscriptionStatus = stringAt(ent, "status")
	if snap.Email == "" {
		snap.Email = c.emailOf()
	}
	return snap, nil
}

func (c *httpClient) emailOf() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.email
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
