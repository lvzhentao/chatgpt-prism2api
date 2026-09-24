package api

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"prism-2api/internal/adapter"
)

// ---------- 内置工具 → 官方 Claude Code 工具映射 ----------
//
// 部分上游在 agent 模式下注入 Shell/Read/Edit/Write/Delete/Glob/Grep/LS/Fetch/
// WebSearch/Task/AskQuestion 等。官方 Claude Code（Ant-native）本地注册表与此
// 不对齐，且几乎所有工具都是 z.strictObject：多一个未知字段、少一个必填字段，
// 或名字大小写不对，客户端都会拒绝执行。
//
// 源码实证（Claude-Code-main）：
//   - findToolByName 大小写敏感，只认注册表里的 name/aliases
//   - 执行时查的是本地全量工具池（含 deferred），不是请求里的 tools[]
//   - Ant 构建 hasEmbeddedSearchTools() 为真时移除 Glob/Grep，提示改用 Bash 的 find/grep
//   - 不存在 LS / Search / Delete；Task 是 Agent 的 alias
//   - WebFetch.prompt 必填；AskUserQuestion 是 questions[{question,header,options[2-4]}]
//   - Read/Edit/Write 用 file_path，不是 path
//
// RequestContext 把工作区报成 "/"，Cursor 因此给出 path="/"。那是工作区根，
// 不是用户机器的文件系统根，转发给 Claude Code 时必须改成 cwd。
//
// 官方 CLI 卸掉 Grep/Glob 后，Cursor 仍会发这两把工具。拦不住上游，只能按本轮
// tools[] 自适应：客户端声明了就原样转发；否则把结构化参数编成 grep -E / find
// （-E 覆盖 ugrep 包装函数前置的 -G；prune + head 避免扫爆 node_modules）。

// builtinToolAliases Cursor 内置名 → 各客户端常见名（按优先级）。
// 只在客户端 tools[] 里出现时才选用，沿用对方自己的大小写。
var builtinToolAliases = map[string][]string{
	"Shell": {"Bash", "bash", "shell", "Shell", "run_command", "execute_command",
		"run_terminal_command", "terminal", "powershell", "PowerShell", "cmd", "execute"},
	"Read": {"Read", "read", "read_file", "readFile", "ReadFile", "view_file", "view"},
	"Edit": {"Edit", "edit", "edit_file", "apply_patch", "apply_diff", "str_replace_editor",
		"replace_in_file", "search_replace"},
	"Write":     {"Write", "write", "write_file", "write_to_file", "create_file", "writeFile", "WriteFile"},
	"Delete":    {"Delete", "delete_file", "deleteFile", "rm"},
	"Glob":      {"Glob", "glob", "find_files", "list_files_glob"},
	"Grep":      {"Grep", "grep", "ripgrep", "search_files"},
	"Search":    {"Grep", "grep", "ripgrep", "search_files"},
	"LS":        {"LS", "ls", "list_dir", "list_files", "list_directory", "listFiles"},
	"Fetch":     {"WebFetch", "fetch", "web_fetch", "fetch_url", "webfetch"},
	"WebSearch": {"WebSearch", "web_search", "webSearch", "search_web"},
	"Task": {"Agent", "Task", "task", "subagent", "run_subagent", "dispatch_agent",
		"delegate"},
	"subagent": {"Agent", "Task", "task", "subagent", "run_subagent", "dispatch_agent",
		"delegate"},
	"AskQuestion": {"AskUserQuestion", "ask_question", "ask", "ask_user"},
}

// shellToolNames 可承接合成 grep/find/ls/rm 的客户端 shell 工具。
var shellToolNames = []string{
	"Bash", "bash", "shell", "Shell", "run_command", "execute_command",
	"run_terminal_command", "terminal", "powershell", "PowerShell", "cmd", "execute",
}

// clientToolSet 客户端本轮声明的工具名 + JSON schema。
// 有声明时：只映射到声明里的名字，参数投影到对方 schema。
// 无声明时：按官方 Claude Code 注册表兜底（延迟工具 / tools=[]）。
type clientToolSet struct {
	names  map[string]string          // lower → 客户端原样大小写
	schema map[string]json.RawMessage // lower → parameters
}

func newToolIndex() *clientToolSet {
	return &clientToolSet{names: map[string]string{}, schema: map[string]json.RawMessage{}}
}

func clientToolIndex(req *ChatCompletionRequest) *clientToolSet {
	idx := newToolIndex()
	if req == nil {
		return idx
	}
	for _, t := range req.Tools {
		n := strings.TrimSpace(t.Function.Name)
		if n == "" {
			continue
		}
		lower := strings.ToLower(n)
		idx.names[lower] = n
		if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
			idx.schema[lower] = t.Function.Parameters
		}
	}
	return idx
}

func indexFromNames(names map[string]bool) *clientToolSet {
	idx := newToolIndex()
	for n, ok := range names {
		if !ok || strings.TrimSpace(n) == "" {
			continue
		}
		idx.names[strings.ToLower(n)] = n
	}
	return idx
}

func (idx *clientToolSet) empty() bool {
	return idx == nil || len(idx.names) == 0
}

func (idx *clientToolSet) pick(candidates ...string) (string, bool) {
	if idx.empty() {
		return "", false
	}
	for _, c := range candidates {
		if orig, ok := idx.names[strings.ToLower(c)]; ok {
			return orig, true
		}
	}
	return "", false
}

func (idx *clientToolSet) schemaOf(name string) json.RawMessage {
	if idx.empty() || name == "" {
		return nil
	}
	return idx.schema[strings.ToLower(name)]
}

func (idx *clientToolSet) shellName() string {
	if orig, ok := idx.pick(shellToolNames...); ok {
		return orig
	}
	if idx.empty() {
		return "Bash"
	}
	return ""
}

// builtinToolNames Cursor 内置工具名集合（客户端声明同名工具时跳过 mcp_tools，
// 避免与内置工具重名导致服务端静默失败）。
// 注意：Agent/Artifact 是 Claude Code 客户端工具，不在内置名单内，走 mcp_tools
// 声明后模型可正常调用（若服务端按云端 subagent 处理则回执快速结束）。
var builtinToolNames = map[string]bool{
	"shell": true, "read": true, "edit": true, "write": true, "delete": true,
	"glob": true, "grep": true, "ls": true, "fetch": true, "webfetch": true,
	"websearch": true, "task": true, "askquestion": true, "switchmode": true,
	"generateimage": true, "recordscreen": true, "computeruse": true,
	"sendtouser": true, "reflect": true, "createplan": true, "updatetodos": true,
	"readtodos": true, "semsearch": true, "listmcpresources": true,
	"readmcpresource": true, "applyagentdiff": true, "blamebyfilepath": true,
	"getmcptools": true, "mcpauth": true, "await": true, "writeshellstdin": true,
	"setupvmenvironment": true, "setactivebranch": true, "sendmessage": true,
	"sendfinalsummary": true, "communicateupdate": true, "truncated": true,
}

func isBuiltinToolName(name string) bool {
	return builtinToolNames[strings.ToLower(strings.TrimSpace(name))]
}

// clientToolNames 兼容旧调用：只保留声明名（小写）。
func clientToolNames(req *ChatCompletionRequest) map[string]bool {
	idx := clientToolIndex(req)
	out := make(map[string]bool, len(idx.names))
	for k := range idx.names {
		out[k] = true
	}
	return out
}

// mapBuiltinToolCall 测试入口（只有名字、没有 schema）。
func mapBuiltinToolCall(tc *adapter.ToolCallInfo, clientTools map[string]bool) (string, string, bool) {
	return mapCursorToolCall(tc, indexFromNames(clientTools))
}

// mapCursorToolCall 把 Cursor 内置调用映射为当前客户端能执行的名字 + 参数。
//
// 规则：
//  1. 客户端声明了对应/别名工具 → 用它的原名，参数投影到它的 schema
//  2. 文件搜索/列表/删除且客户端有 shell 类工具 → 合成到那个 shell（不一定叫 Bash）
//  3. 客户端没声明任何工具 → 官方 Claude Code 注册表兜底
//  4. 客户端声明了工具但完全对不上 → 不转发（避免发明对方没有的名字）

// gateUpstreamToolCall 是 S1 门：上游（沙箱侧）发起的工具调用，先问客户端有没有。
//
// 背景：上游沙箱永远带着 shell/file 工具；模型看到"看下项目进度"这类请求就会去翻
// 它自己那个空工作区（只有 AGENTS.md），再一本正经地让用户"切换项目工作区"——
// 它说的"工作区"永远是它自己的沙箱，不可能是客户本地目录（网关没把客户项目发给它）。
// 想要"读客户本地项目"，唯一正确路径是客户端声明的工具（Codex/Cursor 类在本地执行）。
//
// 规则：
//  1. 客户端声明了对应/别名/shell 合成目标 → 转发（沿用 mapCursorToolCall，不变）。
//  2. 对不上 → 返回 ok=false：调用方必须吞掉该帧，不得发 tool_calls。
//     同时把"工具名+参数摘要"记进 blocked 串，供调用方以受限块形式补进上下文，
//     模型据此收敛回答（而不是凭空编造工作区内容）。
//  3. 客户端 tools 为空 → 沿用旧语义（官方注册表兜底转发），不拦截。
func gateUpstreamToolCall(tc *adapter.ToolCallInfo, idx *clientToolSet) (name, args, blocked string, ok bool) {
	if tc == nil {
		return "", "", "", false
	}
	// 硬拦截：这些工具只在云端沙箱有意义（录屏/计算机操作/生图/沙箱管理），
	// 客户端 tools 为空也不能按注册表兜底转发——客户端永远执行不了。
	if neverForwardTool(tc.ToolName) {
		summary := strings.TrimSpace(tc.ToolName)
		if a := strings.TrimSpace(tc.ArgsJSON); a != "" && a != "{}" {
			if len(a) > 300 {
				a = a[:300] + "…"
			}
			summary += "(" + a + ")"
		}
		return "", "", summary, false
	}
	if mapped, mappedArgs, mok := mapCursorToolCall(tc, idx); mok {
		return mapped, mappedArgs, "", true
	}

	if idx.empty() {
		// tools 为空：旧语义兜底（见 mapCursorToolCall 规则 3），不拦截。
		return tc.ToolName, tc.ArgsJSON, "", true
	}
	summary := strings.TrimSpace(tc.ToolName)
	if a := strings.TrimSpace(tc.ArgsJSON); a != "" && a != "{}" {
		if len(a) > 300 {
			a = a[:300] + "…"
		}
		summary += "(" + a + ")"
	}
	return "", "", summary, false
}

// neverForwardTool 永不对客户端转发的工具名（只在云端沙箱有意义）。
func neverForwardTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "computeruse", "computer_use", "recordscreen", "record_screen",
		"generateimage", "generate_image", "sendtouser", "sendmessage",
		"sendfinalsummary", "communicateupdate", "truncated":
		return true
	}
	return false
}

// blockedToolNote 把被拦截的上游调用拼成受限说明块。
// 空返回 ""（调用方直接跳过）。正文措辞只陈述事实（"客户端没有该工具，
// 本次未执行"），不编造结果、不替模型做结论。
func blockedToolNote(blocked []string) string {
	if len(blocked) == 0 {
		return ""
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(blocked))
	for _, b := range blocked {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		uniq = append(uniq, b)
	}
	if len(uniq) == 0 {
		return ""
	}
	return "[上游工具受限] 以下工具调用因客户端未提供对应工具而未执行，" +
		"不要假设它们已执行、不要编造其结果：\n- " + strings.Join(uniq, "\n- ")
}

// stripEmptyWorkspaceClaims 剪掉"空工作区自证"类句子。
// 模型翻完自己那个空沙箱后常说"当前工作区只有 AGENTS.md / 没有页面代码"，
// 这句话对 API 客户端是误导（它说的永远是它自己的沙箱，不是客户项目）。
// 只做句子级删除：命中任一模式的句子整句去掉，其余正文原样保留。
// 中英文标点都认；整段全被剪空返回 ""（调用方跳过，不追加空块）。
func stripEmptyWorkspaceClaims(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	patterns := []string{
		"AGENTS.md",
		"codex_workspace",
		"当前工作区只有",
		"当前工作区仅",
		"工作区只有",
		"工作区仅",
		"没有页面源码",
		"没有页面代码",
		"没有构建产物",
		"没有改动记录",
		"无法评估",
		"请切换到",
		"切换项目工作区",
		"项目工作区",
		"当前工作区没有",
		"工作区中没有",
		"工作区里没有",
		"workspace",
	}
	lower := strings.ToLower(text)
	hit := false
	for _, p := range patterns {
		if strings.Contains(text, p) || strings.Contains(lower, strings.ToLower(p)) {
			hit = true
			break
		}
	}
	if !hit {
		return text
	}
	// 按句切分：中英文句末标点 + 换行都算边界。注意 "." 也是文件名的组成部分
	// （如 AGENTS.md），只把它当"句末"会把句子切碎——切碎不影响正确性：
	// 碎片只要含模式词照样丢掉，不含的碎片拼回去仍是原文（Join 用 ""）。
	splitRe := regexp.MustCompile(`[^。！？!?\n]+[。！？!?\n]?`)
	var kept []string
	for _, sent := range splitRe.FindAllString(text, -1) {
		drop := false
		for _, p := range patterns {
			if strings.Contains(sent, p) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, sent)
		}
	}
	out := strings.TrimSpace(strings.Join(kept, ""))
	// 兜底：如果切分逻辑一个字都没留下但原文有非模式内容，原样返回（宁可漏剪不断句）。
	if out == "" && strings.TrimSpace(text) != "" {
		// 全段都是模式句 → 真空，返回 "" 让调用方跳过。
		return ""
	}
	return out
}
func mapCursorToolCall(tc *adapter.ToolCallInfo, idx *clientToolSet) (string, string, bool) {
	if idx == nil {
		idx = newToolIndex()
	}
	name := strings.TrimSpace(tc.ToolName)
	args := tc.ArgsJSON
	if name == "" {
		return name, args, false
	}
	lower := strings.ToLower(name)
	// 别名双向归一：上游按沙箱习惯叫（apply_patch/read_file），客户端按自己习惯
	// 声明（Edit/Read）——先归一到表键再解析，否则反向（上游叫别名、客户端声明键名）
	// 会在 resolveDeclared 里查空表，白白被 S1 门拦掉。解析失败不提前返回：
	// lower 归一后继续往下走，让 grep/glob/ls/delete 的 shell 合成分支照常命中。
	if canon := canonicalBuiltinName(name); canon != "" && canon != name {
		if target, ok := resolveDeclared(canon, idx); ok {
			return emitDeclared(idx, target, name, args)
		}
		lower = strings.ToLower(canon)
	}

	if isGrepName(lower) && grepShouldBeGlob(args) {
		if target, ok := resolveDeclared("Glob", idx); ok {
			return emitConverted(idx, target, args)
		}
		if n, a, ok := emitShell(idx, args, globToBashCommand); ok {
			return n, a, true
		}
	}

	if target, ok := resolveDeclared(name, idx); ok {
		if strings.EqualFold(target, "Search") {
			if n, a, ok := emitShell(idx, args, grepToBashCommand); ok {
				return n, a, true
			}
			return name, args, false
		}
		// 客户端用同一个名字声明了这个工具（只差大小写）：没有 Cursor 内置形态要翻译，
		// 参数只投影到它自己的 schema。必须跳过内置转换——apply_patch/read/edit 这些名字
		// 会走 Cursor 形态的参数重写，与客户端 schema 不同名的参数被剪光：Codex 把
		// apply_patch 声明成 freeform 的 {"input": string}，convertEditArgs 后只剩 "{}"。
		return emitDeclared(idx, target, name, args)
	}

	switch lower {
	case "grep", "search":
		return emitShell(idx, args, grepToBashCommand)
	case "glob":
		return emitShell(idx, args, globToBashCommand)
	case "ls":
		return emitShell(idx, args, lsToBashCommand)
	case "delete":
		return emitShell(idx, args, deleteToBashCommand)
	}

	if !idx.empty() {
		return name, args, false
	}

	// tools=[]：Claude Code 延迟工具 / 未声明，按官方注册表转发。
	switch lower {
	case "shell", "bash":
		return emitConverted(idx, "Bash", args)
	case "read":
		return emitConverted(idx, "Read", args)
	case "edit":
		return emitConverted(idx, "Edit", args)
	case "write":
		return emitConverted(idx, "Write", args)
	case "fetch", "webfetch":
		return emitConverted(idx, "WebFetch", args)
	case "websearch":
		return emitConverted(idx, "WebSearch", args)
	case "task", "subagent", "agent":
		return emitConverted(idx, "Agent", args)
	case "askquestion", "askuserquestion":
		return emitConverted(idx, "AskUserQuestion", args)
	case "updatetodos", "todowrite":
		return emitConverted(idx, "TodoWrite", args)
	}
	if aliases := lookupAliases(name); len(aliases) > 0 {
		return emitConverted(idx, aliases[0], args)
	}
	return name, convertForTarget(name, args), false
}

func emitConverted(idx *clientToolSet, target, args string) (string, string, bool) {
	converted := convertForTarget(target, args)
	converted = projectArgsToSchema(converted, idx.schemaOf(target))
	return target, converted, true
}

// emitDeclared 处理「调用名 name 命中客户端声明的工具 target」这一情形。
//
// 名字相同（只差大小写）：调用本来就是冲这个声明来的，没有 Cursor 内置形态要翻译，
// 参数只投影到客户端自己的 schema。必须跳过内置转换——apply_patch/read/edit 这些名字
// 会走 Cursor 形态的参数重写，与客户端 schema 不同名的参数被剪光：Codex 把 apply_patch
// 声明成 freeform 的 {"input": string}，convertEditArgs 后只剩 "{}"。
//
// 名字不同（上游按沙箱习惯叫 apply_patch，客户端声明的是 Edit）：语义就是要翻译，
// 保持上游的内置转换。
func emitDeclared(idx *clientToolSet, target, name, args string) (string, string, bool) {
	if strings.EqualFold(target, name) {
		return target, projectArgsToSchema(args, idx.schemaOf(target)), true
	}
	return emitConverted(idx, target, args)
}

func emitShell(idx *clientToolSet, argsJSON string, synth func(string) (string, bool)) (string, string, bool) {
	sh := idx.shellName()
	if sh == "" {
		return "", "", false
	}
	cmd, ok := synth(argsJSON)
	if !ok {
		return "", "", false
	}
	return sh, projectArgsToSchema(cmd, idx.schemaOf(sh)), true
}

func mapOutgoingToolName(name string, clientTools map[string]bool) string {
	return mapOutgoingToolNameIdx(name, indexFromNames(clientTools))
}

func mapOutgoingToolNameIdx(name string, idx *clientToolSet) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return trimmed
	}
	mapped, _, ok := mapCursorToolCall(&adapter.ToolCallInfo{ToolName: trimmed, ArgsJSON: "{}"}, idx)
	if ok && mapped != "" {
		return mapped
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "grep", "search", "glob", "ls", "delete":
		if sh := idx.shellName(); sh != "" {
			return sh
		}
	}
	return trimmed
}

func isGrepName(lower string) bool {
	return lower == "grep" || lower == "search"
}

// skipPartialToolStart Grep/Glob/LS/Delete/Write 的最终名字与参数依赖完整帧
// （可能改写成 Bash/Glob，Write 则由分片聚合后才有完整参数），提前
// content_block_start 会把错误名字/空 input 钉死——空 Write 块会让客户端
// 执行 Write 直接报 "file_path missing, content missing"。
func skipPartialToolStart(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "grep", "search", "glob", "ls", "delete", "write":
		return true
	}
	return false
}

func resolveDeclared(cursorName string, idx *clientToolSet) (string, bool) {
	if idx.empty() {
		return "", false
	}

	for _, a := range lookupAliases(cursorName) {
		if orig, ok := idx.pick(a); ok {
			// 返回客户端声明的原样大小写（omp 声明 read/glob/grep 小写，
			// Claude Code 声明 Read/Glob 大写）——返回别名候选会把大小写带偏，
			// 客户端按名查工具时 "Tool Read not found"。
			return orig, true
		}
	}
	return idx.pick(cursorName)
}

// canonicalBuiltinName 双向归一：别名表里的任何成员（含键、含各客户端写法）
// 都归到表键（如 apply_patch/str_replace_editor/read_file → Edit/Read…）。
// 上游按自己习惯叫（apply_patch），客户端按自己习惯声明（Edit），双向都必须通。
// 不在表内返回 ""（调用方按原名走）。
func canonicalBuiltinName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	if _, ok := builtinToolAliases[trimmed]; ok {
		return trimmed
	}
	for k, aliases := range builtinToolAliases {
		if strings.EqualFold(k, trimmed) {
			return k
		}
		for _, a := range aliases {
			if strings.EqualFold(a, trimmed) {
				return k
			}
		}
	}
	return ""
}

func lookupAliases(cursorName string) []string {
	if aliases, ok := builtinToolAliases[cursorName]; ok {
		return aliases
	}
	for k, aliases := range builtinToolAliases {
		if strings.EqualFold(k, cursorName) {
			return aliases
		}
	}
	return nil
}

func grepShouldBeGlob(argsJSON string) bool {
	m, ok := parseArgsMap(argsJSON)
	if !ok || len(m) == 0 {
		return false
	}
	pattern := strField(m, "pattern", "query")
	outputMode := strField(m, "output_mode")
	if !looksLikeFilenameGlob(pattern) {
		return false
	}
	return !strings.EqualFold(outputMode, "content")
}

func looksLikeFilenameGlob(pattern string) bool {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return false
	}
	if strings.Contains(p, "**") {
		return true
	}
	if strings.HasPrefix(p, "*.") && !strings.ContainsAny(p, `^$[]()|\`) {
		return true
	}
	if strings.Contains(p, "/") && strings.ContainsAny(p, "*?") {
		return true
	}
	return false
}

// bashPathArg Cursor 工作区根 "/" → 客户端 cwd。
func bashPathArg(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "."
	}
	return p
}

const defaultSearchHeadLimit = 250

func grepToBashCommand(argsJSON string) (string, bool) {
	m, ok := parseArgsMap(argsJSON)
	if !ok || len(m) == 0 {
		return "", false
	}
	pattern := strField(m, "pattern", "query")
	if pattern == "" {
		return "", false
	}
	outputMode := strField(m, "output_mode")
	if looksLikeFilenameGlob(pattern) && !strings.EqualFold(outputMode, "content") {
		return globToBashCommand(argsJSON)
	}
	path := bashPathArg(strField(m, "path"))
	glob := strField(m, "glob")
	// -E：GNU grep 与 ugrep 都能吃分组；官方 CLI 的 grep 函数会前置 -G，
	// 用户参数在后，后出现的 -E 覆盖基本正则。
	flags := "-E -r"
	switch {
	case strings.Contains(outputMode, "files_with_matches"):
		flags += " -l"
	case strings.EqualFold(outputMode, "count"):
		flags += " -c"
	default:
		flags += " -n"
	}
	if boolField(m, "-i", "case_insensitive") {
		flags += " -i"
	}
	cmd := "grep " + flags +
		" --exclude-dir=node_modules --exclude-dir=.git"
	if glob != "" {
		cmd += " --include=" + shellQuote(glob)
	}
	cmd += " " + shellQuote(pattern) + " " + shellQuote(path)
	cmd = withHead(cmd, searchHeadLimit(m))
	return bashToolArgs(cmd, "Search file contents")
}

func globToBashCommand(argsJSON string) (string, bool) {
	m, ok := parseArgsMap(argsJSON)
	if !ok || len(m) == 0 {
		return "", false
	}
	pattern := strField(m, "pattern", "glob")
	if pattern == "" {
		return "", false
	}
	path := bashPathArg(strField(m, "path"))
	cmd := withHead(globPatternToFind(path, pattern), searchHeadLimit(m))
	return bashToolArgs(cmd, "Find files by pattern")
}

func globPatternToFind(path, pattern string) string {
	name := pattern
	if i := strings.LastIndex(pattern, "/"); i >= 0 {
		if base := pattern[i+1:]; base != "" && base != "**" {
			name = base
		}
	}
	prefix := "find " + shellQuote(path) + ` \( -name node_modules -o -name .git \) -prune -o`
	if name == "*" || name == "**" || name == "**/*" || pattern == "**/*" {
		return prefix + " -type f -print"
	}
	return prefix + " -type f -name " + shellQuote(name) + " -print"
}

func searchHeadLimit(m map[string]any) int {
	if n, ok := intField(m, "head_limit"); ok {
		if n == 0 {
			return 0
		}
		if n < 0 {
			return defaultSearchHeadLimit
		}
		return n
	}
	return defaultSearchHeadLimit
}

func withHead(cmd string, limit int) string {
	if limit <= 0 {
		return cmd
	}
	return cmd + " | head -n " + strconv.Itoa(limit)
}

func lsToBashCommand(argsJSON string) (string, bool) {
	path := "."
	if m, ok := parseArgsMap(argsJSON); ok {
		if p := strField(m, "path"); p != "" {
			path = bashPathArg(p)
		}
	}
	cmd := "ls -la"
	if path != "." {
		cmd += " " + shellQuote(path)
	}
	return bashToolArgs(cmd, "List directory")
}

func deleteToBashCommand(argsJSON string) (string, bool) {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return "", false
	}
	path := strField(m, "path", "file_path")
	if path == "" || path == "/" {
		return "", false
	}
	return bashToolArgs("rm -f "+shellQuote(path), "Delete file")
}

func bashToolArgs(cmd, desc string) (string, bool) {
	b, err := json.Marshal(map[string]any{
		"command":     cmd,
		"description": desc,
		"timeout":     30000,
	})
	if err != nil {
		return "", false
	}
	return string(b), true
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// convertToolArgs 兼容旧测试：按 Cursor kind 转成对应客户端 schema。
func convertToolArgs(kind, argsJSON string) string {
	target := kind
	switch strings.ToLower(kind) {
	case "shell":
		target = "Bash"
	case "grep":
		target = "Grep"
	case "ask_question":
		target = "AskUserQuestion"
	case "web_search":
		target = "WebSearch"
	case "fetch":
		target = "WebFetch"
	case "subagent":
		target = "Agent"
	case "read":
		target = "Read"
	case "write":
		target = "Write"
	case "edit":
		target = "Edit"
	}
	return convertForTarget(target, argsJSON)
}

func convertForTarget(target, argsJSON string) string {
	if argsJSON == "" {
		return argsJSON
	}
	switch strings.ToLower(target) {
	case "bash", "shell", "run_command", "execute_command", "run_terminal_command",
		"terminal", "powershell", "cmd", "execute":
		return convertBashArgs(argsJSON)
	case "read", "read_file", "readfile", "view_file", "view":
		return convertReadArgs(argsJSON)
	case "edit", "edit_file", "apply_patch", "apply_diff", "str_replace_editor",
		"replace_in_file", "search_replace":
		return convertEditArgs(argsJSON)
	case "write", "write_file", "write_to_file", "create_file", "writefile":
		return convertWriteArgs(argsJSON)
	case "grep", "ripgrep", "search_files":
		return convertGrepArgs(argsJSON)
	case "glob", "find_files", "list_files_glob":
		return convertGlobArgs(argsJSON)
	case "websearch", "web_search", "search_web":
		return convertWebSearchArgs(argsJSON)
	case "webfetch", "fetch", "web_fetch", "fetch_url":
		return convertWebFetchArgs(argsJSON)
	case "agent", "task", "subagent", "delegate":
		return convertAgentArgs(argsJSON)
	case "askuserquestion", "askquestion", "ask_question", "ask", "ask_user":
		return convertAskUserQuestionArgs(argsJSON)
	case "todowrite":
		return convertTodoWriteArgs(argsJSON)
	default:
		return argsJSON
	}
}

func convertBashArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	if v, exists := m["timeout_ms"]; exists {
		delete(m, "timeout_ms")
		if _, has := m["timeout"]; !has {
			m["timeout"] = v
		}
	}
	return marshalArgs(keepFields(m, "command", "timeout", "description", "run_in_background"), argsJSON)
}

func convertReadArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	renameField(m, "path", "file_path")
	return marshalArgs(keepFields(m, "file_path", "offset", "limit", "pages"), argsJSON)
}

func convertEditArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	renameField(m, "path", "file_path")
	renameField(m, "oldString", "old_string")
	renameField(m, "old_str", "old_string")
	renameField(m, "newString", "new_string")
	renameField(m, "new_str", "new_string")
	return marshalArgs(keepFields(m, "file_path", "old_string", "new_string", "replace_all"), argsJSON)
}

func convertWriteArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	renameField(m, "path", "file_path")
	renameField(m, "contents", "content")
	return marshalArgs(keepFields(m, "file_path", "content"), argsJSON)
}

func convertGrepArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	renameField(m, "query", "pattern")
	return marshalArgs(keepFields(m, "pattern", "path", "glob", "output_mode",
		"-B", "-A", "-C", "context", "-n", "-i", "type", "head_limit", "offset", "multiline"), argsJSON)
}

func convertGlobArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	if strField(m, "pattern") == "" {
		renameField(m, "glob", "pattern")
	}
	if p := strField(m, "path"); p == "/" {
		delete(m, "path")
	}
	return marshalArgs(keepFields(m, "pattern", "path"), argsJSON)
}

func convertWebSearchArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	renameField(m, "search_term", "query")
	return marshalArgs(keepFields(m, "query", "allowed_domains", "blocked_domains"), argsJSON)
}

func convertWebFetchArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	if strField(m, "prompt") == "" {
		m["prompt"] = "Extract the main content as markdown. Include headings and key information."
	}
	return marshalArgs(keepFields(m, "url", "prompt"), argsJSON)
}

var cursorAgentTypes = map[string]string{
	"generalpurpose":  "general-purpose",
	"general-purpose": "general-purpose",
	"explore":         "Explore",
	"plan":            "Plan",
	"verification":    "verification",
}

func convertAgentArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	prompt := strField(m, "prompt", "task", "instruction")
	desc := strField(m, "description")
	subType := strField(m, "subagent_type", "type", "agent_type")
	if mapped := mapAgentType(subType); mapped != "" {
		subType = mapped
	}
	if subType == "" {
		if mapped := mapAgentType(desc); mapped != "" {
			subType = mapped
			desc = ""
		}
	}
	if prompt == "" {
		prompt = "Complete the delegated task."
	}
	if desc == "" {
		desc = shortAgentDesc(prompt, subType)
	}
	out := map[string]any{}
	if desc != "" {
		out["description"] = desc
	}
	if prompt != "" {
		out["prompt"] = prompt
	}
	if subType != "" {
		out["subagent_type"] = subType
	}
	for _, k := range []string{"model", "name", "run_in_background"} {
		if v, exists := m[k]; exists {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return argsJSON
	}
	return marshalArgs(out, argsJSON)
}

func mapAgentType(s string) string {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	return cursorAgentTypes[key]
}

func shortAgentDesc(prompt, subType string) string {
	if subType != "" {
		return subType
	}
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return "subagent task"
	}
	if len(fields) > 5 {
		fields = fields[:5]
	}
	return strings.Join(fields, " ")
}

func convertAskUserQuestionArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	if alreadyAskUserQuestion(m) {
		return marshalArgs(keepFields(m, "questions", "answers", "annotations", "metadata"), argsJSON)
	}

	title := strField(m, "question", "title")
	if title == "" {
		title = "Which option do you prefer?"
	}
	optionStrs := collectOptionStrings(m)
	switch {
	case len(optionStrs) == 0:
		optionStrs = []string{"Yes", "No"}
	case len(optionStrs) == 1:
		optionStrs = append(optionStrs, "Skip")
	case len(optionStrs) > 4:
		optionStrs = optionStrs[:4]
	}

	options := make([]any, 0, len(optionStrs))
	for _, s := range optionStrs {
		label := s
		if r := []rune(label); len(r) > 40 {
			label = string(r[:40])
		}
		options = append(options, map[string]any{"label": label, "description": s})
	}
	header := title
	if r := []rune(header); len(r) > 12 {
		header = string(r[:12])
	}

	return marshalArgs(map[string]any{
		"questions": []any{
			map[string]any{
				"question": title,
				"header":   header,
				"options":  options,
			},
		},
	}, argsJSON)
}

func alreadyAskUserQuestion(m map[string]any) bool {
	qs, ok := m["questions"].([]any)
	if !ok || len(qs) == 0 {
		return false
	}
	q0, ok := qs[0].(map[string]any)
	if !ok {
		return false
	}
	_, hasQ := q0["question"]
	_, hasH := q0["header"]
	_, hasO := q0["options"]
	return hasQ && hasH && hasO
}

func collectOptionStrings(m map[string]any) []string {
	var out []string
	appendVal := func(v any) {
		switch t := v.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case map[string]any:
			if s := strField(t, "label", "text", "question"); s != "" {
				out = append(out, s)
			}
		}
	}
	if qs, ok := m["questions"].([]any); ok {
		for _, q := range qs {
			appendVal(q)
		}
	}
	if opts, ok := m["options"].([]any); ok {
		for _, o := range opts {
			appendVal(o)
		}
	}
	return out
}

func convertTodoWriteArgs(argsJSON string) string {
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	return marshalArgs(keepFields(m, "todos"), argsJSON)
}

// argSynonymGroups 各客户端对同一语义的字段名。投影时把 Cursor 字段填进 schema 里出现的那个。
var argSynonymGroups = [][]string{
	{"file_path", "path", "filePath", "target_file", "file", "filename"},
	{"command", "cmd"},
	{"timeout", "timeout_ms", "timeoutMs"},
	{"pattern", "regex", "search_pattern"},
	{"url", "uri", "href"},
	{"prompt", "instruction"},
	{"description", "desc"},
	{"old_string", "oldString", "old_str"},
	{"new_string", "newString", "new_str"},
	{"query", "search_term", "q"},
	{"content", "contents"},
}

var requiredArgDefaults = map[string]any{
	"prompt": "Extract the main content as markdown. Include headings and key information.",
}

type toolJSONSchema struct {
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties any                        `json:"additionalProperties"`
}

func projectArgsToSchema(argsJSON string, schema json.RawMessage) string {
	if len(schema) == 0 || string(schema) == "null" || argsJSON == "" {
		return argsJSON
	}
	var spec toolJSONSchema
	if err := json.Unmarshal(schema, &spec); err != nil || len(spec.Properties) == 0 {
		return argsJSON
	}
	m, ok := parseArgsMap(argsJSON)
	if !ok {
		return argsJSON
	}
	out := make(map[string]any, len(spec.Properties))
	for prop := range spec.Properties {
		if v, exists := m[prop]; exists {
			out[prop] = v
			continue
		}
		for _, syn := range synonymsOf(prop) {
			if v, exists := m[syn]; exists {
				out[prop] = v
				break
			}
		}
	}
	for _, req := range spec.Required {
		if _, exists := out[req]; exists {
			continue
		}
		if d, ok := requiredArgDefaults[req]; ok {
			out[req] = d
		}
	}
	return marshalArgs(out, argsJSON)
}

func synonymsOf(field string) []string {
	for _, g := range argSynonymGroups {
		for _, n := range g {
			if n == field {
				out := make([]string, 0, len(g)-1)
				for _, x := range g {
					if x != field {
						out = append(out, x)
					}
				}
				return out
			}
		}
	}
	return nil
}

func parseArgsMap(argsJSON string) (map[string]any, bool) {
	s := strings.TrimSpace(argsJSON)
	if s == "" || s == "null" {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, false
	}
	if m == nil {
		return map[string]any{}, true
	}
	return m, true
}

func marshalArgs(m map[string]any, fallback string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return fallback
	}
	return string(b)
}

func keepFields(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

func renameField(m map[string]any, from, to string) {
	if _, exists := m[to]; exists {
		delete(m, from)
		return
	}
	if v, ok := m[from]; ok {
		m[to] = v
		delete(m, from)
	}
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func boolField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b
		case string:
			if strings.EqualFold(b, "true") || b == "1" {
				return true
			}
		}
	}
	return false
}

func intField(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return int(i), true
			}
		}
	}
	return 0, false
}
