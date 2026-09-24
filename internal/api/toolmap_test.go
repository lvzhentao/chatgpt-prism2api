package api

import (
	"encoding/json"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

func TestMapBuiltinToolCallWithoutClientTools(t *testing.T) {
	empty := map[string]bool{}
	cases := []struct {
		builtin string
		want    string
	}{
		{"Shell", "Bash"},
		{"Read", "Read"},
		{"Edit", "Edit"},
		{"Write", "Write"},
		{"WebSearch", "WebSearch"},
		{"Fetch", "WebFetch"},
		{"subagent", "Agent"},
		{"Task", "Agent"},
		{"AskQuestion", "AskUserQuestion"},
	}
	for _, c := range cases {
		name, _, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
			Kind:     strings.ToLower(c.builtin),
			ToolName: c.builtin,
			ArgsJSON: `{"command":"ls","url":"https://example.com","prompt":"go","query":"ab"}`,
		}, empty)
		if !ok {
			t.Fatalf("%s: expected ok=true (must forward, not drop)", c.builtin)
		}
		if name != c.want {
			t.Fatalf("%s: mapped to %q, want %q", c.builtin, name, c.want)
		}
	}
	// Grep/Search/Glob：官方 Ant 构建没有这些工具。无 pattern 不转发。
	for _, g := range []struct{ name, args string }{
		{"Grep", `{"command":"ls"}`},
		{"Search", `{"command":"ls"}`},
		{"Glob", `{"command":"ls"}`},
	} {
		if _, _, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
			Kind:     "grep",
			ToolName: g.name,
			ArgsJSON: g.args,
		}, empty); ok {
			t.Fatalf("%s with non-search args must not forward", g.name)
		}
	}
	for _, g := range []struct{ name, args, wantCmd string }{
		{"Grep", `{"pattern":"foo"}`, `grep -E -r -n --exclude-dir=node_modules --exclude-dir=.git 'foo' '.'`},
		{"Search", `{"pattern":"foo"}`, `grep -E -r -n --exclude-dir=node_modules --exclude-dir=.git 'foo' '.'`},
		{"Glob", `{"pattern":"*.go"}`, `find '.' \\( -name node_modules -o -name .git \\) -prune -o -type f -name '*.go' -print`},
		{"LS", `{"path":"/tmp"}`, `ls -la '/tmp'`},
		{"Delete", `{"path":"tmp.txt"}`, `rm -f 'tmp.txt'`},
	} {
		name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
			Kind:     strings.ToLower(g.name),
			ToolName: g.name,
			ArgsJSON: g.args,
		}, empty)
		if !ok || name != "Bash" || !strings.Contains(args, g.wantCmd) {
			t.Fatalf("%s must synthesize Bash %q: %q %q ok=%v", g.name, g.wantCmd, name, args, ok)
		}
	}
}

func TestMapBuiltinToolCallUnknownToolStillPassthrough(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "custom",
		ToolName: "SomeWeirdTool",
		ArgsJSON: `{"a":1}`,
	}, map[string]bool{})
	if ok {
		t.Fatalf("unknown tool must keep ok=false, got ok=true")
	}
	if name != "SomeWeirdTool" || args != `{"a":1}` {
		t.Fatalf("passthrough changed: %q %q", name, args)
	}
}

func TestMapGrepToBashCommand(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"remapToolID","path":"internal","output_mode":"files_with_matches"}`,
	}, map[string]bool{})
	if !ok || name != "Bash" {
		t.Fatalf("grep must synthesize Bash, got %q ok=%v %s", name, ok, args)
	}
	if !strings.Contains(args, `grep -E -r -l --exclude-dir=node_modules --exclude-dir=.git 'remapToolID' 'internal'`) {
		t.Fatalf("bash command wrong: %s", args)
	}
	if !strings.Contains(args, `"timeout":30000`) {
		t.Fatalf("bash args must include timeout: %s", args)
	}

	name, args, ok = mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Search",
		ArgsJSON: `{"pattern":"TODO","path":"src"}`,
	}, map[string]bool{})
	if !ok || name != "Bash" || !strings.Contains(args, `grep -E -r -n --exclude-dir=node_modules --exclude-dir=.git 'TODO' 'src'`) {
		t.Fatalf("search must synthesize Bash: %q %q ok=%v", name, args, ok)
	}

	_, args, ok = mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"it's"}`,
	}, map[string]bool{})
	if !ok || !strings.Contains(args, `'it'\\''s'`) {
		t.Fatalf("quote escaping wrong: %s", args)
	}

	_, _, ok = mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"path":"/"}`,
	}, map[string]bool{})
	if ok {
		t.Fatalf("empty pattern must not synthesize")
	}
}

func TestGrepNeverDefaultsToSearch(t *testing.T) {
	name, _, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"foo"}`,
	}, map[string]bool{})
	if !ok || name == "Search" || name == "Grep" {
		t.Fatalf("Ant-native default must not be Search/Grep, got %q ok=%v", name, ok)
	}
	if name != "Bash" {
		t.Fatalf("want Bash, got %q", name)
	}
}

func TestDeclaredSearchStillBecomesBash(t *testing.T) {
	name, _, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Search",
		ArgsJSON: `{"pattern":"foo"}`,
	}, map[string]bool{"search": true})
	if ok && strings.EqualFold(name, "Search") {
		t.Fatalf("Search is not a Claude Code tool, got %q", name)
	}
}

func TestGrepGlobPatternUsesFindNotGrep(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"**/*","path":"/","output_mode":"files_with_matches"}`,
	}, map[string]bool{})
	if !ok || name != "Bash" {
		t.Fatalf("glob-like grep must become Bash find, got %q ok=%v", name, ok)
	}
	if strings.Contains(args, "grep ") {
		t.Fatalf("must not grep a glob pattern: %s", args)
	}
	if !strings.Contains(args, `find '.' \\( -name node_modules -o -name .git \\) -prune -o -type f -print`) {
		t.Fatalf("want pruned find cwd, got %s", args)
	}
	if strings.Contains(args, `'/'`) {
		t.Fatalf("workspace root / must not be forwarded as filesystem root: %s", args)
	}
}

func TestGrepShellSynthesisUsesEREAndPrunesModules(t *testing.T) {
	_, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"app.(get|post)","path":"src","-i":true}`,
	}, map[string]bool{})
	if !ok {
		t.Fatal("expected synthesis")
	}
	if !strings.Contains(args, `grep -E -r -n -i --exclude-dir=node_modules --exclude-dir=.git 'app.(get|post)' 'src'`) {
		t.Fatalf("want ERE grep that prunes node_modules: %s", args)
	}
	if !strings.Contains(args, `| head -n 250`) {
		t.Fatalf("default head_limit must cap output: %s", args)
	}

	_, args, ok = mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"foo","head_limit":0}`,
	}, map[string]bool{})
	if !ok || strings.Contains(args, "head -n") {
		t.Fatalf("head_limit=0 must be unlimited: %s ok=%v", args, ok)
	}
}

func TestWorkspaceRootPathRewrittenForGrep(t *testing.T) {
	_, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"TODO","path":"/"}`,
	}, map[string]bool{})
	if !ok || !strings.Contains(args, `grep -E -r -n --exclude-dir=node_modules --exclude-dir=.git 'TODO' '.'`) {
		t.Fatalf("path=/ must become cwd: %s ok=%v", args, ok)
	}
}

func TestMapBuiltinToolCallClientDeclaredWins(t *testing.T) {
	name, _, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "shell",
		ToolName: "Shell",
		ArgsJSON: `{"command":"ls"}`,
	}, map[string]bool{"run_command": true})
	if !ok || name != "run_command" {
		t.Fatalf("declared alias should win, got %q ok=%v", name, ok)
	}
	name, _, ok = mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"query":"foo"}`,
	}, map[string]bool{"grep": true})
	if !ok || name != "grep" {
		t.Fatalf("declared Grep should keep client's declared casing, got %q ok=%v", name, ok)
	}
}

func TestDeclaredGlobWinsForGlobLikeGrep(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "grep",
		ToolName: "Grep",
		ArgsJSON: `{"pattern":"**/*.go","path":"src","output_mode":"files_with_matches"}`,
	}, map[string]bool{"glob": true})
	if !ok || name != "glob" {
		t.Fatalf("client with glob should get declared casing, got %q ok=%v", name, ok)
	}
	if !strings.Contains(args, `"pattern":"**/*.go"`) {
		t.Fatalf("glob args: %s", args)
	}
}

func TestConvertGrepArgsQueryToPattern(t *testing.T) {
	got := convertToolArgs("grep", `{"query":"TODO","path":"src","output_mode":"content","extra":1}`)
	if !strings.Contains(got, `"pattern":"TODO"`) || strings.Contains(got, `"query"`) {
		t.Fatalf("grep args not converted: %s", got)
	}
	if !strings.Contains(got, `"path":"src"`) {
		t.Fatalf("path must pass through: %s", got)
	}
	if strings.Contains(got, `"extra"`) {
		t.Fatalf("strictObject extra field leaked: %s", got)
	}
}

func TestConvertReadPathToFilePathAndStripExtra(t *testing.T) {
	got := convertForTarget("Read", `{"path":"internal/api/toolmap.go","offset":1,"limit":20,"unknown":true}`)
	if !strings.Contains(got, `"file_path":"internal/api/toolmap.go"`) {
		t.Fatalf("path→file_path: %s", got)
	}
	if strings.Contains(got, `"path"`) || strings.Contains(got, `"unknown"`) {
		t.Fatalf("must strip path/unknown for Read strictObject: %s", got)
	}
}

func TestConvertEditPathToFilePath(t *testing.T) {
	got := convertForTarget("Edit", `{"path":"a.go","old_string":"x","new_string":"y"}`)
	if !strings.Contains(got, `"file_path":"a.go"`) || strings.Contains(got, `"path"`) {
		t.Fatalf("edit path→file_path: %s", got)
	}
}

func TestConvertWebFetchRequiresPrompt(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "fetch",
		ToolName: "Fetch",
		ArgsJSON: `{"url":"https://example.com"}`,
	}, map[string]bool{})
	if !ok || name != "WebFetch" {
		t.Fatalf("Fetch→WebFetch: %q ok=%v", name, ok)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	if m["url"] != "https://example.com" {
		t.Fatalf("url: %v", m["url"])
	}
	prompt, _ := m["prompt"].(string)
	if prompt == "" {
		t.Fatalf("WebFetch.prompt is required: %s", args)
	}
}

func TestConvertAskUserQuestionSchema(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "ask_question",
		ToolName: "AskQuestion",
		ArgsJSON: `{"title":"Which library?","questions":["std","third-party"]}`,
	}, map[string]bool{})
	if !ok || name != "AskUserQuestion" {
		t.Fatalf("got %q ok=%v", name, ok)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["question"]; has {
		t.Fatalf("top-level question is invalid for CC schema: %s", args)
	}
	qs, ok := m["questions"].([]any)
	if !ok || len(qs) != 1 {
		t.Fatalf("want questions[1], got %s", args)
	}
	q0 := qs[0].(map[string]any)
	if q0["question"] != "Which library?" {
		t.Fatalf("question: %v", q0["question"])
	}
	if _, ok := q0["header"].(string); !ok {
		t.Fatalf("header required: %s", args)
	}
	opts, ok := q0["options"].([]any)
	if !ok || len(opts) < 2 || len(opts) > 4 {
		t.Fatalf("options must be 2-4: %s", args)
	}
	o0 := opts[0].(map[string]any)
	if o0["label"] == nil || o0["description"] == nil {
		t.Fatalf("option needs label+description: %s", args)
	}
}

func TestConvertAgentSubagentType(t *testing.T) {
	name, args, ok := mapBuiltinToolCall(&adapter.ToolCallInfo{
		Kind:     "subagent",
		ToolName: "subagent",
		ArgsJSON: `{"description":"generalPurpose","prompt":"Find the auth middleware"}`,
	}, map[string]bool{})
	if !ok || name != "Agent" {
		t.Fatalf("subagent→Agent: %q ok=%v", name, ok)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	if m["subagent_type"] != "general-purpose" {
		t.Fatalf("subagent_type: %v (%s)", m["subagent_type"], args)
	}
	if m["prompt"] != "Find the auth middleware" {
		t.Fatalf("prompt: %v", m["prompt"])
	}
	if m["description"] == "generalPurpose" {
		t.Fatalf("raw Cursor type leaked as description: %s", args)
	}
}

func TestMapOutgoingToolName(t *testing.T) {
	empty := map[string]bool{}
	if got := mapOutgoingToolName("Shell", empty); got != "Bash" {
		t.Fatalf("Shell→Bash, got %q", got)
	}
	if got := mapOutgoingToolName("Grep", empty); got != "Bash" {
		t.Fatalf("Grep partial→Bash, got %q", got)
	}
	if got := mapOutgoingToolName("Grep", map[string]bool{"grep": true}); got != "grep" {
		t.Fatalf("declared Grep keeps client casing, got %q", got)
	}
	if !skipPartialToolStart("Grep") || !skipPartialToolStart("LS") {
		t.Fatal("Grep/LS partial start must be skipped")
	}
	if !skipPartialToolStart("Write") {
		t.Fatal("Write partial start must be skipped: empty-input tool_use block makes the client report 'file_path missing'")
	}
	if skipPartialToolStart("Shell") {
		t.Fatal("Shell partial start should not be skipped")
	}
}

func TestAnthropicDeferredToolDefsExtracted(t *testing.T) {
	body := `{
		"model": "claude-opus-4-8",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": "deferred tools available"},
			{"name": "Monitor", "description": "watch a task", "input_schema": {"type": "object", "properties": {"task_id": {"type": "string"}}}},
			{"name": "WebSearch", "description": "search the web", "input_schema": {"type": "object"}}
		]
	}`
	var req AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out, err := AnthropicToOpenAIRequest(&req, nil, "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range out.Tools {
		names[tl.Function.Name] = true
	}
	if !names["Monitor"] || !names["WebSearch"] {
		t.Fatalf("deferred tool defs not extracted: %v", names)
	}
	for _, m := range out.Messages {
		if m.Role == "" {
			t.Fatalf("role-less message leaked into messages: %+v", m)
		}
	}
}

func TestClineStyleDeclaredTools(t *testing.T) {
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{
			{Type: "function", Function: ToolFunction{
				Name:       "read_file",
				Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			}},
			{Type: "function", Function: ToolFunction{
				Name:       "execute_command",
				Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			}},
		},
	})
	name, args, ok := mapCursorToolCall(&adapter.ToolCallInfo{
		Kind: "read", ToolName: "Read", ArgsJSON: `{"path":"pkg/foo.go","offset":1,"unknown":true}`,
	}, idx)
	if !ok || name != "read_file" {
		t.Fatalf("Read→read_file, got %q ok=%v", name, ok)
	}
	if !strings.Contains(args, `"path":"pkg/foo.go"`) || strings.Contains(args, `"file_path"`) || strings.Contains(args, `"unknown"`) {
		t.Fatalf("schema projection failed: %s", args)
	}

	name, args, ok = mapCursorToolCall(&adapter.ToolCallInfo{
		Kind: "grep", ToolName: "Grep", ArgsJSON: `{"pattern":"TODO","path":"src"}`,
	}, idx)
	if !ok || name != "execute_command" {
		t.Fatalf("Grep→execute_command, got %q ok=%v", name, ok)
	}
	if !strings.Contains(args, `grep -E -r -n --exclude-dir=node_modules --exclude-dir=.git 'TODO' 'src'`) {
		t.Fatalf("synthesized command: %s", args)
	}
	if strings.Contains(args, `"timeout"`) || strings.Contains(args, `"description"`) {
		t.Fatalf("must strip fields not in execute_command schema: %s", args)
	}
}

func TestUnknownClientDoesNotInventClaudeCodeTools(t *testing.T) {
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "my_custom_tool"}}},
	})
	name, _, ok := mapCursorToolCall(&adapter.ToolCallInfo{
		Kind: "grep", ToolName: "Grep", ArgsJSON: `{"pattern":"foo"}`,
	}, idx)
	if ok {
		t.Fatalf("must not invent Bash/Grep for a client that declared neither, got %q", name)
	}
}

func TestConvertWriteArgsKeepsContentsSynonym(t *testing.T) {
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:       "Write",
			Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`),
		}}},
	})
	_, args, ok := mapCursorToolCall(&adapter.ToolCallInfo{
		Kind: "write", ToolName: "Write", ArgsJSON: `{"path":"/tmp/CLAUDE.md","contents":"# hi"}`,
	}, idx)
	if !ok {
		t.Fatal("Write should map")
	}
	if !strings.Contains(args, `"content":"# hi"`) || strings.Contains(args, `"contents"`) {
		t.Fatalf("contents must rename to content: %s", args)
	}
	if !strings.Contains(args, `"file_path":"/tmp/CLAUDE.md"`) {
		t.Fatalf("path must rename to file_path: %s", args)
	}
}

func TestGateUpstreamToolCallForwardsDeclared(t *testing.T) {
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "Read"}}},
	})
	name, _, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
		Kind: "read", ToolName: "read", ArgsJSON: `{"path":"/x"}`,
	}, idx)
	if !ok || blocked != "" || name != "Read" {
		t.Fatalf("声明过的工具必须转发: name=%q blocked=%q ok=%v", name, blocked, ok)
	}
}

func TestGateUpstreamToolCallBlocksUndeclared(t *testing.T) {
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather"}}},
	})
	_, _, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
		Kind: "shell", ToolName: "shell", ArgsJSON: `{"command":"ls /codex_workspace"}`,
	}, idx)
	if ok {
		t.Fatal("客户端没有 shell，shell 调用必须拦截")
	}
	if blocked == "" || !strings.Contains(blocked, "shell") {
		t.Fatalf("拦截摘要必须带工具名: %q", blocked)
	}
}

func TestGateUpstreamToolCallEmptyToolsPassthrough(t *testing.T) {
	_, _, blocked, ok2 := gateUpstreamToolCall(&adapter.ToolCallInfo{
		Kind: "read", ToolName: "read", ArgsJSON: `{"path":"/x"}`,
	}, newToolIndex())
	if !ok2 || blocked != "" {
		t.Fatal("tools 为空沿用旧兜底语义，不得拦截")
	}
}

func TestStripEmptyWorkspaceClaims(t *testing.T) {
	in := "页面已有雏形。当前工作区只有 AGENTS.md，没有页面源码、构建产物或改动记录。请切换到 cosex-translate 项目工作区，或提供当前页面截图和代码。"
	got := stripEmptyWorkspaceClaims(in)
	if strings.Contains(got, "AGENTS.md") || strings.Contains(got, "切换") {
		t.Fatalf("空工作区自证句子必须剪掉: %q", got)
	}
	if !strings.Contains(got, "页面已有雏形") {
		t.Fatalf("正常句子必须保留: %q", got)
	}
	if stripEmptyWorkspaceClaims("今天天气不错，适合出门。") != "今天天气不错，适合出门。" {
		t.Fatal("无模式正文必须原样返回")
	}
	if stripEmptyWorkspaceClaims("当前工作区只有 AGENTS.md。") != "" {
		t.Fatal("全段都是自证时应返回空")
	}
}

func TestBlockedToolNote(t *testing.T) {
	if blockedToolNote(nil) != "" || blockedToolNote([]string{"", "  "}) != "" {
		t.Fatal("空输入必须返回空")
	}
	got := blockedToolNote([]string{"shell(ls /x)", "shell(ls /x)", "read(/y)"})
	if !strings.Contains(got, "shell(ls /x)") || !strings.Contains(got, "read(/y)") {
		t.Fatalf("note 必须列出拦截项: %q", got)
	}
	if strings.Count(got, "shell(ls /x)") != 1 {
		t.Fatalf("note 必须去重: %q", got)
	}
}

func TestCanonicalAliasBidirectional(t *testing.T) {
	// 上游叫 apply_patch（别名），客户端声明 Edit（键名）→ 必须转发成 Edit。
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:       "Edit",
			Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}`),
		}}},
	})
	name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
		Kind: "function_call", ToolName: "apply_patch",
		ArgsJSON: `{"path":"/a.go","old_str":"x","new_str":"y"}`,
	}, idx)
	if !ok || blocked != "" || name != "Edit" {
		t.Fatalf("apply_patch 必须归一到 Edit: name=%q blocked=%q ok=%v", name, blocked, ok)
	}
	for _, want := range []string{`"file_path":"/a.go"`, `"old_string":"x"`, `"new_string":"y"`} {
		if !strings.Contains(args, want) {
			t.Fatalf("参数未投影到 Edit schema: %s 缺 %s", args, want)
		}
	}
}

func TestGateNeverForwardToolsEvenWithEmptyClient(t *testing.T) {
	for _, name := range []string{"computeruse", "recordscreen", "generateimage", "sendtouser"} {
		_, _, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
			Kind: "function_call", ToolName: name, ArgsJSON: `{}`,
		}, newToolIndex())
		if ok || blocked == "" {
			t.Fatalf("%s 即使客户端 tools 为空也必须拦截: ok=%v blocked=%q", name, ok, blocked)
		}
	}
}

func TestGateShellSynthStillWorksThroughGate(t *testing.T) {
	// 客户端只声明 Bash，上游叫 ls → 合成 bash 命令（不是拦截）。
	idx := clientToolIndex(&ChatCompletionRequest{
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:       "Bash",
			Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		}}},
	})
	name, args, blocked, ok := gateUpstreamToolCall(&adapter.ToolCallInfo{
		Kind: "tool_call", ToolName: "ls", ArgsJSON: `{"path":"/tmp"}`,
	}, idx)
	if !ok || blocked != "" || name != "Bash" {
		t.Fatalf("ls 应合成 Bash: name=%q blocked=%q ok=%v", name, blocked, ok)
	}
	if !strings.Contains(args, "ls -la") {
		t.Fatalf("合成命令丢失: %s", args)
	}
}
