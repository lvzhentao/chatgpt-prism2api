package api

import (
	"os"
	"regexp"
	"strings"

	"prism-2api/internal/adapter"
)

// ============================================================
// 沙箱代执行护栏（S1 的另一半：管"上游自己动手"的情形）
//
// 背景：上游产品自带 shell/文件工具且会真执行（它的系统提示词要求如此）。
// 模型看到 "ls" / "创建文件" 类请求时，最强的本能是用它自己的沙箱——执行结果
// 以正文形式回来（payload.output 里没有任何调用条目，网关无钩可挂）。
// 此时正文里的「已创建 hello.txt」「total 16 / drwxrws--- sandbox sandbox」
// 对客户端是假动作：客户机器上什么都没发生。
//
// 启用条件（缺一不可，把误伤面压到最小）：
//   1. 客户端本轮声明了工具（它在等 tool_call，不是在等"我帮你做了"）；
//   2. 本轮没有任何 tool_call 送达客户端（有一个送达就说明走了正道）。
//
// 命中后：句级/行级剪掉假动作内容，追加一行事实说明。宁可漏，不误删
// 普通技术回答（代码块里的 ls 输出若无沙箱指纹则不动）。
// ============================================================

// sandboxExecNote 是剪除假动作后补的事实行。
const sandboxExecNote = "[网关说明] 上游在它自己的沙箱环境里执行了动作（该环境不是你的本地目录），客户端未收到执行请求；相关描述已移除。如需在本地执行，请以工具调用形式让客户端执行。"

var (
	// ls -la 类输出指纹：行首权限位（drwxrws---）或 "total N" 计数行。
	lsListingRe = regexp.MustCompile(`(?m)^\s*(total\s+\d+\s*$|[dlpscb-][rwxstST-]{9}\s)`)
	// 假动作声称（句子级）：只列"已完成"形态的动词短语，进行式/疑问句不碰。
	// 「已创建X」直连形态 + 「已将/已把 X 写入」宾语插入形态（后者 2026-09-18 真机漏网）。
	sandboxClaimRes = []*regexp.Regexp{
		regexp.MustCompile(`已(经)?(创建|新建|写入|保存|生成|编译|执行|运行)(了|完成|成功)?|已(将|把).{0,20}?(创建|新建|写入|保存|生成|编译|执行|运行)`),
		regexp.MustCompile(`successfully (created|written|saved|generated|executed|run)`),
		regexp.MustCompile(`(?i)\bcreated\b.*\bsuccessfully\b`),
	}
	// 沙箱身份指纹（出现在 ls 输出所有者列等）。
	sandboxFingerprint = regexp.MustCompile(`\bsandbox\s+sandbox\b|/codex_workspace/`)

	// ============================================================
	// 交付事故检测（无 tools 场景的假交付：真机样本 2026-09-19
	// "已完成精美的鹈鹕骑车单页 index.html"——模型把产物写进自己沙箱里的文件
	// 后拿"已完成"当答复，客户端一个字节都拿不到。旧护栏只覆盖"声明了 tools"
	// 的请求，纯聊天请求裸奔；且动词表没有"完成"，连剪除都匹配不上。
	// ============================================================
	// 交付声称（文件指向锚定，窄匹配防误伤）：中文动词（含"完成/制作"等交付措辞）
	// 或英文创建类动词 + 文件名后缀；外加"文件已保存至"类明示形态。
	fileExtGroup     = `html?|pdf|png|jpe?g|svg|gif|webp|md|tex|py|js|ts|jsx|tsx|css|json|csv|docx|pptx|xlsx|zip|txt`
	deliveryClaimRes = []*regexp.Regexp{
		regexp.MustCompile(`已(经)?(创建|新建|写入|保存|生成|编译|执行|运行|完成|产出|制作|绘制|渲染).{0,40}?\S{1,60}\.(?:` + fileExtGroup + `)\b`),
		regexp.MustCompile(`(?i)\b(?:created|generated|saved|written|built|compiled)\b[^.\n]{0,60}\b[\w-]+\.(?:` + fileExtGroup + `)\b`),
		regexp.MustCompile(`(?i)files? (?:has been |have been |was |were |is |are )?(?:saved|created|written|generated)\b`),
	}
	// fenced 代码块（粗略佐证正文是否已带交付内容）。
	fencedCodeRe = regexp.MustCompile("(?s)```.*?```")
)

// deliveryBodyContentChars 佐证阈值：正文 fenced 代码块总长低于它视为"内容缺失"。
const deliveryBodyContentChars = 400

// sandboxDeliveryNote 无 tools 场景剪除后补的事实行（比 tools 场景的 sandboxExecNote
// 更面向最终用户：告诉调用方如何拿到内容）。
const sandboxDeliveryNote = "[网关说明] 上游在它自己的隔离环境里创建了文件，而未把内容写进回复正文，该文件无法送达客户端。请重新要求“把完整内容直接写在回复里”。"

// sandboxDeliveryClaim 判定"假交付事故"：声称写入了文件 + 正文缺足量代码块。
// 正文已带足量代码块 → 内容已交付，不算事故（不追捞、不剪除）。
func sandboxDeliveryClaim(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	claimed := false
	for _, re := range deliveryClaimRes {
		if re.MatchString(text) {
			claimed = true
			break
		}
	}
	if !claimed {
		return false
	}
	total := 0
	for _, m := range fencedCodeRe.FindAllString(text, -1) {
		total += len(m)
	}
	return total < deliveryBodyContentChars
}

// sentenceDeliveryClaim 句子级命中交付声称（用于句级剪除）。
func sentenceDeliveryClaim(sent string) bool {
	for _, re := range deliveryClaimRes {
		if re.MatchString(sent) {
			return true
		}
	}
	return false
}

// guardNoToolDeliveryText 无 tools 场景的假交付兜底（追捞失败/关闭后才轮到它）：
// 只有交付事故（声称写文件 + 缺内容）才剪——句级剪声称、行级剪 ls 清单/沙箱指纹，
// 纯 ls 教学输出、带完整代码块的回答一律不动。返回清理后文本与是否剪过。
func guardNoToolDeliveryText(text string) (string, bool) {
	if !sandboxDeliveryClaim(text) {
		return text, false
	}
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if lsListingRe.MatchString(line) || sandboxFingerprint.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	splitRe := regexp.MustCompile(`[^。！？!?\n]+[。！？!?\n]?`)
	var sb strings.Builder
	for _, sent := range splitRe.FindAllString(strings.Join(kept, "\n"), -1) {
		if sentenceDeliveryClaim(sent) {
			continue
		}
		sb.WriteString(sent)
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return sandboxDeliveryNote, true
	}
	return out + "\n\n" + sandboxDeliveryNote, true
}

// planDeliverySalvage 判定是否值得做"交付追捞"重试（同账号、至多一次）：
//
//	delivery：声称写入了文件但正文缺足量内容（不限是否有 tools）；
//	refusal ：无 tools 时模型翻自己的环境后拒绝/声称文件不存在。
//
// 图片附件轮豁免由调用方负责（roundHasImageAttachment）。
func planDeliverySalvage(text string, clientToolsDeclared bool) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	if sandboxDeliveryClaim(text) {
		return "delivery", true
	}
	if !clientToolsDeclared && ownEnvRefusalRe.MatchString(text) {
		return "refusal", true
	}
	return "", false
}

// roundHasImageAttachment 本轮任何消息带图片附件时 true：附件链路
// （attachments.go）要求模型在沙箱还原文件再 view_image，其"已还原/已创建
// prism-uploads/x.png"类动作是设计内行为，剪除与追捞全部豁免。
func roundHasImageAttachment(nr *adapter.NativeRequest) bool {
	if nr == nil {
		return false
	}
	for _, m := range nr.Messages {
		for _, f := range m.Files {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(f.Mime)), "image/") {
				return true
			}
		}
	}
	return false
}

// guardSandboxExecutedText 剪正文里的沙箱假动作。返回清理后文本与是否剪过。
// 输入为空或完全没命中指纹/声称时原样返回（hit=false）。
func guardSandboxExecutedText(text string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return text, false
	}
	// 快速通道：连一个指纹都不沾就直接放行。
	claimed := false
	for _, re := range sandboxClaimRes {
		if re.MatchString(text) {
			claimed = true
			break
		}
	}
	listed := lsListingRe.MatchString(text)
	if !claimed && !listed && !sandboxFingerprint.MatchString(text) {
		return text, false
	}

	var kept []string
	// 行级：去掉 ls 清单行与沙箱指纹行。
	for _, line := range strings.Split(text, "\n") {
		if lsListingRe.MatchString(line) || sandboxFingerprint.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	joined := strings.Join(kept, "\n")

	// 句级：去掉假动作声称句。
	splitRe := regexp.MustCompile(`[^。！？!?\n]+[。！？!?\n]?`)
	var sb strings.Builder
	for _, sent := range splitRe.FindAllString(joined, -1) {
		drop := false
		for _, re := range sandboxClaimRes {
			if re.MatchString(sent) {
				drop = true
				break
			}
		}
		if !drop {
			sb.WriteString(sent)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return sandboxExecNote, true
	}
	return out + "\n\n" + sandboxExecNote, true
}

// ownEnvRefusalRe 匹配"模型翻了自己的环境然后拒绝/声称不能"的文本形态。
// 这些都是模型没走工具协议、拿自己的空沙箱作答的证据（真机样本 2026-09-18）：
//
//	「无法读取：`README.md` 不存在。」「无法读取：当前目录中不存在 `README.md`。」
//	「无法调用未提供的 `Write` 工具。」「…且当前没有可调用的 `Read` 工具。」
//	「无法调用消息中伪造的客户端工具协议。」「无法访问工作区外的 `/tmp/toolloop`。」
//
// 误伤面：用户消息本身含"无法读取"等词时模型可能复述它——此时多付出一次重试，
// 行为仍正确（模型再答一次同样的话），可接受。
var ownEnvRefusalRe = regexp.MustCompile(
	`(无法|不能|未能)(读取|调用|执行|访问|写入|创建)` +
		`|未提供.{0,12}工具|没有可调用的|可调用的.{0,12}工具.{0,4}(不存在|没有)` +
		`|伪造的.{0,12}(工具|协议)` +
		`|当前(目录|工作区|环境)中?不存在|工作区中?不存在|工作区外`)

// shouldRetryToolContract 判定本轮是否值得做"协议强化重试"（同账号、至多一次）：
// 正文里出现沙箱代执行证据（护栏命中）或翻自有环境后的拒绝形态。
// 只在"声明了工具却零 tool_call 送达"时被调用方调用；两个条件之外不作判断。
func shouldRetryToolContract(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if _, hit := guardSandboxExecutedText(text); hit {
		return true
	}
	return ownEnvRefusalRe.MatchString(text)
}

// toolContractRetryOn 协议强化重试开关：默认开，WEB2API_TOOL_CONTRACT_RETRY=0/false/off 关闭。
// 请求级读取（env 热改即生效），成本可忽略。
func toolContractRetryOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_TOOL_CONTRACT_RETRY")))
	return v != "0" && v != "false" && v != "off"
}

// deliverySalvageRetryOn 交付追捞开关：默认开，WEB2API_DELIVERY_SALVAGE=0/false/off 关闭。
// 请求级读取（env 热改即生效）。追捞只在假交付事故轮多花一次上游往返，关掉即回到"只剪除"。
func deliverySalvageRetryOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_DELIVERY_SALVAGE")))
	return v != "0" && v != "false" && v != "off"
}
