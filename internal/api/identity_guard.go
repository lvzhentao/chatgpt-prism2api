package api

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"

	"prism-2api/internal/emulation"
)

// ============================================================
// 身份闸（P1）：GPT 入口（/v1/chat/completions + /v1/responses）的回答侧兜底。
//
// 背景：上游服务端自带一套内置提示词（自称 Codex / GPT-5），注入压不住的三类
// 情况——tools 路径忽略 system（I8）、user 块内硬改身份不可靠（I6/I7）——
// 只能靠回答后清洗。门控刻意收窄：只在「本轮就是身份话题」时改写，普通问答
// （包括聊 Codex/Claude 区别的正常问题）一条不动。
// ============================================================

// defaultIdentityAnswer 是身份闸的默认回退答句（与注入指令的身份规则同口径）。
const defaultIdentityAnswer = "我是本服务的通用助手。"

// identityTopicRe 识别「短问句是否在问身份」：主体词（你/您/your）+ 属性词（身份/模型/供应商/点名底座）
// 双条件组合判定。ENHANCE-PLAN P1-1 改法 A：旧版整串锚定 + 白名单词表漏掉了「由谁提供/哪家公司」类问法。
// 主体词约束挡住「OpenAI 是哪家公司」这类正常问答（无"你/您/your"指向自身）。
// 已知踩点：「你的 GPT 配额还剩多少」会被当成身份话题——但这类问句本就该按本站口径答，
// 改写成身份答句反而更安全（见 ENHANCE-PLAN P1-1 风险节），故接受。
var identitySubjectRe = regexp.MustCompile(`(?i)(你|您|your|yourself)`)
var identityAttrRe = regexp.MustCompile(
	`(?i)(谁|什么模型|什么版本|哪家公司|哪家厂商|哪个公司|哪个厂商|由谁提供|谁提供|谁开发的|谁做的|` +
		`what model|which model|who (made|built|provides|owns)|based on what|` +
		`gpt|claude|openai|anthropic|codex|底座|供应商|开发方|提供商)`)

// identityGreetingRe 旧版整串锚定的短问（你好/hi/自我介绍一下等无属性词的纯寒暄），保留作兜底。
var identityGreetingRe = regexp.MustCompile(`(?i)^\s*(who are you[?？]?|你是谁[呀啊吗]?[?？]?|what is your name[?？]?|你叫什么[?？]?|what are you[?？]?|自我介绍一下[?？]?|which model are you[?？]?|你是什么模型[?？]?|hi[?？]?|hello[?？]?|你好[?？]?)\s*$`)

// identityTopicMaxChars 身份话题的最大问句长度（navos 的 IDENTITY_TOPIC_MAX_CHARS 口径）。
const identityTopicMaxChars = 60

var baseModelLeakRes = []*regexp.Regexp{
	// 第一人称自称 Codex（"我是 Codex…"是本站真机实测的泄漏原文）。
	regexp.MustCompile(`(?i)(i\s+am|i['’]m|my name is|我是|我叫)\W{0,10}codex`),
	// 第一人称/关系型自述 GPT 底座。
	regexp.MustCompile(`(?i)(i\s+am|i['’]m|my name is|我是|我叫|基于|based on|built on|powered)\W{0,10}gpt`),
	regexp.MustCompile(`(?i)基于\s*(?:openai|codex)`),
	regexp.MustCompile(`(?i)我是由\s*(?:openai|gpt)`),
	// 关系型自述 OpenAI 出品（裸 openai 一词不算，避免误伤正常讨论）。
	regexp.MustCompile(`(?i)\bby\s+openai\b`),
	regexp.MustCompile(`(?i)由\s*openai|来自\s*openai|from\s+openai`),
	regexp.MustCompile(`(?i)openai\s*(打造|开发|训练|创建|制作|出品)|(?:打造|开发|训练|创建|制作|出品)\s*[^，。！？；\n]{0,12}openai`),
	// 实测泄漏原句里的固定搭配。
	regexp.MustCompile(`AI\s*编程与协作代理`),
}

// hasBaseModelLeak 检查回答是否自述底座（Codex / GPT-x / OpenAI 出品）。
func hasBaseModelLeak(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range baseModelLeakRes {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// sanitizeBaseModelText 按段落删掉自述底座的句子（只删不加，删空返回空串）。
func sanitizeBaseModelText(text string) string {
	parts := regexp.MustCompile(`\n{2,}`).Split(text, -1)
	var kept []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || hasBaseModelLeak(part) {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n\n")
}

// lastChatUserText 取 chat 请求最后一条 user 消息的文本（兼容 string /
// [{type:text}] / [{type:input_text}] 三种 content 形态，responses 转过来的
// input_text 在这里归一）。
func lastChatUserText(req *ChatCompletionRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(req.Messages[i].Role)) == "user" {
			return chatText(req.Messages[i].Content)
		}
	}
	return ""
}

// chatText 尽量把 content 提取为文本：string 直接用；数组取 text/input_text 块拼接。
func chatText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if (p.Type == "text" || p.Type == "input_text") && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
func identityGate(req *ChatCompletionRequest) (question string, ok bool) {
	q := strings.TrimSpace(lastChatUserText(req))
	if q == "" || len([]rune(q)) > identityTopicMaxChars {
		return "", false
	}
	// 双条件：必须同时指向自身（主体词）+ 问身份/模型/供应商（属性词）。
	// 「OpenAI 是哪家公司」（无主体词）与「你们公司社保怎么交」（无属性词）都不命中。
	if identitySubjectRe.MatchString(q) && identityAttrRe.MatchString(q) {
		return q, true
	}
	// 兜底：旧版整串锚定的短问（你好/hi/自我介绍一下等无属性词的纯寒暄）。
	if identityGreetingRe.MatchString(q) {
		return q, true
	}
	return "", false
}

// guardIdentityAnswer 只在 gate 命中时改写回答，其余原样返回。
// 规则：底座自述/拒答/Anthropic 口径泄漏 → 段落清洗 → 仍脏或删空 → 身份答句。
// 返回改写后的文本与动作（passthrough|sanitized|identity，供日志）。
func (s *Server) guardIdentityAnswer(question, text string) (string, string) {
	if question == "" || strings.TrimSpace(text) == "" {
		return text, "passthrough"
	}
	dirty := hasBaseModelLeak(text) || CheckRefusal(text) || emulation.HasIdentityLeak(text)
	if !dirty {
		return text, "passthrough"
	}
	if cleaned := strings.TrimSpace(sanitizeBaseModelText(text)); cleaned != "" &&
		!hasBaseModelLeak(cleaned) && !CheckRefusal(cleaned) && !emulation.HasIdentityLeak(cleaned) {
		return cleaned, "sanitized"
	}
	if cleaned := strings.TrimSpace(emulation.SanitizeIdentityText(text)); cleaned != "" &&
		!hasBaseModelLeak(cleaned) && !CheckRefusal(cleaned) && !emulation.HasIdentityLeak(cleaned) {
		return cleaned, "sanitized"
	}
	answer := defaultIdentityAnswer
	if s != nil {
		if custom := s.runtimeIdentityAnswer(); custom != "" {
			answer = custom
		}
	}
	return answer, "identity"
}

// runtimeIdentityAnswer 读管理端配置的身份答句（空=默认答句）。
func (s *Server) runtimeIdentityAnswer() string {
	if s == nil || s.runtime == nil {
		return ""
	}
	return s.runtime.IdentityAnswer()
}

// runtimeIdentityGuardOn 读身份闸总开关（默认开）。
func (s *Server) runtimeIdentityGuardOn() bool {
	if s == nil || s.runtime == nil {
		return true
	}
	return s.runtime.IdentityGuardOn()
}

// logIdentityGuard 身份闸命中时打一行（passthrough 不打，避免刷屏）。
func logIdentityGuard(action, question string) {
	if action == "passthrough" {
		return
	}
	log.Printf("identity guard: action=%s question=%q", action, truncateQuestion(question, 60))
}

func truncateQuestion(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
