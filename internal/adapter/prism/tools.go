package prism

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"prism-2api/internal/adapter"
)

// ============================================================
// 工具调用仿真。
//
// 上游没有客户端工具通道：真机实测（2026-09-17，tools 平铺 Responses 形状与
// Chat-Completions 嵌套形状各发一次）返回的 payload.output 只有 message/reasoning
// item，模型完全无视 tools 声明，凭记忆直接作答。所以工具只能靠提示词约定 + 文本解析：
//
//	客户端 tools[] → 系统消息里的协议说明 + JSON Schema
//	模型输出 <tool_call>{...}</tool_call> → 解析成内核的 ToolCall 事件
//	客户端回灌 role:"tool" → 已有的历史折叠（"Tool result(工具名): …"）带回去
//
// 一次回复里出现多个块 = 并行工具调用；回灌结果后模型再输出块 = 连续多轮调用。
// ============================================================

// toolCallOpen / toolCallClose 是给模型约定的信封。
const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
)

// toolCallRe 匹配信封；用非贪婪 .*? 到最近的 </tool_call>。
var toolCallRe = regexp.MustCompile(`(?is)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// toolContract 生成注入给模型的工具协议说明。放在最后一条 system 里，
// 与最终 user 消息相邻，遵守率最高。
func toolContract(nr *adapter.NativeRequest) string {
	var sb strings.Builder
	sb.WriteString("[工具协议] 宿主程序在本轮向你提供了下列工具。你需要工具返回的数据时，必须在回复里输出工具调用块，格式严格如下（一行一个，JSON 必须是合法 JSON)：\n")
	sb.WriteString(toolCallOpen)
	sb.WriteString(`{"name":"工具名","arguments":{"参数名":值}}`)
	sb.WriteString(toolCallClose)
	names := toolNamesForExample(nr)
	argKey := toolExampleArgKey(nr)
	sb.WriteString("\n例如（必须用下面「可用工具」里的真实名字，不要改名、不要编新名字；示例里的参数值只是占位，实际值必须按用户消息填写，严禁照抄）：\n")
	sb.WriteString(toolCallOpen)
	sb.WriteString(fmt.Sprintf(`{"name":%s,"arguments":{%s:%s}}`, strconv.Quote(names), strconv.Quote(argKey), strconv.Quote(toolExampleArgValue(argKey))))
	sb.WriteString(toolCallClose)
	sb.WriteString("\n\n可用工具：\n")
	for i, t := range nr.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s", i+1, name)
		if d := strings.TrimSpace(t.Description); d != "" {
			sb.WriteString(" —— " + d)
		}
		sb.WriteString("\n")
		if len(t.Parameters) > 0 {
			schema := strings.TrimSpace(string(t.Parameters))
			if len(schema) > 2000 {
				schema = schema[:2000]
			}
			sb.WriteString("   参数 JSON Schema：" + schema + "\n")
		}
	}
	sb.WriteString("\n规则：\n" +
		"- 需要工具数据就调用，不要凭记忆猜测；能直接回答就不调用。\n" +
		"- 上面「可用工具」由宿主程序提供且已就绪，与你自身环境里的内置工具无关：\n" +
		"  不要检查你是否内置了它们，不要以“未提供该工具”为由拒绝，不要在本地环境代为执行——\n" +
		"  你的执行结果客户端收不到；只有输出工具调用块，客户端才会执行并把结果带回来。\n" +
		"- 路径与文件由客户端判断：路径在工作区内还是外、文件存在与否、是否可写，\n" +
		"  全部由客户端在它自己的机器上验证——你不要审查路径，不要以「工作区外/不可写/文件不存在」为由拒绝；\n" +
		"  创建、写入、修改客户文件也照常发起调用（Write/Edit 等变更类工具同样由客户端真实执行）。\n" +
		"- 交付物（代码/网页/文档）的完整内容必须写在回复正文里，禁止写进你环境里的文件后声称“已创建/已保存”——\n" +
		"  客户端收不到你环境里的文件，只收得到正文和工具调用块。\n" +
		"- 一次回复可以输出多个工具调用块（并行调用），也可以一个都不输出。\n" +
		"- 调用块之外可以写一句简短说明，但不要重复块里的 JSON。\n" +
		"- 宿主已经执行过的工具结果会出现在名为「[已执行工具的结果]」的段落里，格式为 `- 工具名: 内容`。\n" +
		"- 看到该段落就直接使用里面的数据作答，不要声称看不到结果，也不要为同样的数据重复调用工具。\n" +
		"- 只有「[已执行工具的结果]」里没有的数据才需要发起新的调用；拿到结果前不要编造结果。\n" +
		"- 需要多步时：先输出当前这一步的调用，等结果返回后再决定下一轮调用。\n")
	return sb.String()
}

// toolNamesForExample 取首个客户端工具名，用于协议示例的具名填充。
// 抽象占位（"工具名"）会让模型按上游沙箱习惯改名（如 ReadLocalFile→Read）；
// 具名示例把名字钉死。无工具时回退占位（此时示例不会被使用）。
func toolNamesForExample(nr *adapter.NativeRequest) string {
	if nr != nil {
		for _, t := range nr.Tools {
			if name := strings.TrimSpace(t.Name); name != "" {
				return name
			}
		}
	}
	return "工具名"
}

// toolExampleArgKey 取首个工具的 schema 必填首键（Bash→command、Read→file_path），
// 让示例的 arguments 形状与真实 schema 一致；取不到回退通用键。
func toolExampleArgKey(nr *adapter.NativeRequest) string {
	if nr != nil {
		for _, t := range nr.Tools {
			if strings.TrimSpace(t.Name) == "" {
				continue
			}
			var spec struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if json.Unmarshal(t.Parameters, &spec) == nil {
				if len(spec.Required) > 0 {
					return spec.Required[0]
				}
				for k := range spec.Properties {
					return k
				}
			}
			break
		}
	}
	return "path"
}

// toolExampleArgValue 给协议示例的参数值选一个形状真实的占位：
// command 给 "ls -la" 而不是 "README.md"——形状错误的示例会误导模型照抄。
func toolExampleArgValue(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "command", "cmd":
		return "ls -la"
	case "path", "file_path", "filepath", "file", "filename":
		return "README.md"
	case "content", "text", "data":
		return "hello"
	case "query", "q", "keyword":
		return "示例查询"
	case "city":
		return "北京"
	}
	return "值"
}

// toolReinforceBlock 协议强化块：仅在网关判定"模型翻了自己的环境、没走工具协议"
// 并要求同账号重试时（nr.Extra["prism_tool_reinforce"]=="1"）追加到消息末尾。
// 机制与 V4 具名示例一致：不给抽象规则，给逐字形状 + 必须只输出块的硬指令。
func toolReinforceBlock(nr *adapter.NativeRequest) string {
	if nr == nil || nr.Extra["prism_tool_reinforce"] != "1" {
		return ""
	}
	name := toolNamesForExample(nr)
	key := toolExampleArgKey(nr)
	var sb strings.Builder
	sb.WriteString("[协议纠正] 你上一轮没有输出任何工具调用块——这是协议违规。")
	if bad, _ := nr.Extra["prism_tool_reinforce_bad"].(string); strings.TrimSpace(bad) != "" {
		sb.WriteString("你上一轮的回复是：\n「" + truncateRunes(strings.TrimSpace(bad), 300) + "」\n")
	}
	sb.WriteString("这轮回复里，你在自己的沙箱环境里执行了动作，或声称工具未提供/文件不存在：" +
		"你的沙箱与客户端完全隔离，你在里面的任何执行、读取、写入客户端都收不到；" +
		"上面「可用工具」由客户端真实提供、此刻即可调用。\n" +
		"现在重新回答「用户当前消息」，只允许输出一个工具调用块，不要输出任何其他内容（参数值按用户消息填写）：\n")
	sb.WriteString(toolCallOpen + fmt.Sprintf(`{"name":%s,"arguments":{%s:%s}}`,
		strconv.Quote(name), strconv.Quote(key), strconv.Quote(toolExampleArgValue(key))) + toolCallClose)
	return sb.String()
}

// deliverySalvageBlock 交付追捞块：网关判定"模型把产物写进了自己沙箱里的文件"
// （prism_delivery_salvage=delivery）或"翻自己的环境后拒绝/声称文件不存在"
// （prism_delivery_salvage=refusal）并要求同账号重试时，追加到消息末尾。
// 与 toolReinforceBlock 同机制：引用上一轮原文（新会话的模型才有上下文）+ 硬指令。
// 追捞轮与工具协议无关（无 tools 的纯聊天也会触发），所以独立成块。
func deliverySalvageBlock(nr *adapter.NativeRequest) string {
	if nr == nil {
		return ""
	}
	mode, _ := nr.Extra["prism_delivery_salvage"].(string)
	if mode != "delivery" && mode != "refusal" {
		return ""
	}
	var sb strings.Builder
	if mode == "delivery" {
		sb.WriteString("[交付纠正] 你上一轮把产物写进了自己环境里的文件，并以此作为答复。")
	} else {
		sb.WriteString("[交付纠正] 你上一轮翻了自己的环境去找用户提到的文件，然后拒绝或声称文件不存在。")
	}
	if bad, _ := nr.Extra["prism_delivery_salvage_bad"].(string); strings.TrimSpace(bad) != "" {
		sb.WriteString("你上一轮的回复是：\n「" + truncateRunes(strings.TrimSpace(bad), 300) + "」\n")
	}
	if mode == "delivery" {
		sb.WriteString("你的环境与客户端完全隔离，你环境里的文件客户端一个字节都收不到——那个交付是无效的。" +
			"现在重新交付：把该产物的完整内容直接输出在回复正文中（单个代码块，一行不落，不能用文件路径或省略代替）。" +
			"可以读取你环境里刚写入的那个文件把内容抄进正文，也可以重新生成；不要再创建或修改任何文件，不要解释，直接输出内容本身。")
	} else {
		sb.WriteString("用户提到的文件、目录都在他自己的机器上，你的环境里没有它们。" +
			"现在重新回答用户：明确说明你无法直接读取他机器上的文件，请他把文件内容直接粘贴到对话里；不要再去翻你自己的环境。")
	}
	return sb.String()
}

// truncateRunes 按 rune 截断（中文不拦腰），超长加省略号。
func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// extractToolCallsFromText 从模型正文里抽出工具调用块，并返回剥掉这些块之后的正文。
// 解析失败的块按普通文本保留（宁可让客户端看到原文，也不要吞掉内容）。
func extractToolCallsFromText(text string) ([]adapter.StreamedToolCall, string) {
	if !strings.Contains(text, toolCallOpen) {
		return nil, text
	}
	var calls []adapter.StreamedToolCall
	kept := text
	for _, m := range toolCallRe.FindAllStringSubmatch(text, -1) {
		raw := strings.TrimSpace(m[1])
		name, args, ok := parseToolCallJSON(raw)
		if !ok {
			continue
		}
		calls = append(calls, adapter.StreamedToolCall{
			Tool:       name,
			Name:       name,
			ToolCallID: newCallID(),
			RawArgs:    args,
			ToolIndex:  uint32(len(calls)),
			Kind:       "function",
		})
		kept = strings.Replace(kept, m[0], "", 1)
	}
	if len(calls) == 0 {
		return nil, text
	}
	return calls, cleanAfterStrip(kept)
}

// parseToolCallJSON 兼容 {name,arguments} 与 {tool,arguments} 两种键名，
// arguments 既可以是对象也可以是对象字符串。
func parseToolCallJSON(raw string) (string, string, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", "", false
	}
	name := strings.TrimSpace(stringAt(m, "name"))
	if name == "" {
		name = strings.TrimSpace(stringAt(m, "tool"))
	}
	if name == "" {
		return "", "", false
	}
	args := "{}"
	switch v := m["arguments"].(type) {
	case map[string]any:
		if b, err := json.Marshal(v); err == nil {
			args = string(b)
		}
	case string:
		s := strings.TrimSpace(v)
		if s != "" && json.Valid([]byte(s)) {
			args = s
		} else if s != "" {
			if b, err := json.Marshal(map[string]any{"input": s}); err == nil {
				args = string(b)
			}
		}
	case nil:
	default:
		if b, err := json.Marshal(map[string]any{"value": v}); err == nil {
			args = string(b)
		}
	}
	return name, args, true
}

// cleanAfterStrip 剥掉调用块后收拾残留空白（避免正文里留下大段空行）。
func cleanAfterStrip(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// newCallID 生成工具调用 id（客户端要拿它回灌 tool_call_id）。
func newCallID() string {
	return "call_" + strings.ReplaceAll(uuid4(), "-", "")
}
