package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatReq(msgs ...ChatMessage) *ChatCompletionRequest {
	return &ChatCompletionRequest{Model: "m", Messages: msgs}
}

func userMsg(text string) ChatMessage {
	return ChatMessage{Role: "user", Content: json.RawMessage(mustJSON(text))}
}

// 门控：裸身份问句才命中；普通问答、长文、空问句一律不命中。
func TestIdentityGate(t *testing.T) {
	for _, q := range []string{"你是谁", "你是谁呀", "你是谁呀？", "who are you?", "你是什么模型", "hi", "你好",
		// P1-1 provider 类问法（2026-09-18 真机泄漏样本）：必须命中。
		"你是什么模型？由谁提供？", "你由哪家公司提供？", "你是 GPT 吗？", "你是 claude 吗",
		"你是由 OpenAI 提供的吗？"} {
		if _, ok := identityGate(chatReq(userMsg(q))); !ok {
			t.Fatalf("应命中身份话题，got miss: %q", q)
		}
	}
	for _, q := range []string{
		"Codex 和 Claude 有什么区别", // 含底座词的普通问答：绝不能触发（无主体词）
		"OpenAI 是哪家公司？",        // 无主体词：正常任务问答不触发
		"你们公司社保怎么交？",           // 无属性词：不触发
		"你是谁？请用三百字详细介绍你自己，包括你的名字、版本、开发公司和技术特点，越详细越好谢谢，请从你的诞生背景和成长经历开始讲起吧", // 超长问句不触发
		"牛顿第二定律是什么",
		"",
	} {
		if _, ok := identityGate(chatReq(userMsg(q))); ok {
			t.Fatalf("普通问答不应触发身份闸，got hit: %q", q)
		}
	}
	// 多轮对话：只看最后一条 user 消息。
	req := chatReq(userMsg("牛顿第二定律是什么"),
		ChatMessage{Role: "assistant", Content: json.RawMessage(`"F=ma"`)}, userMsg("你是谁"))
	if _, ok := identityGate(req); !ok {
		t.Fatal("多轮对话最后一条是身份问句时应命中")
	}
}

// tools 常驻声明不影响门控（GPT 客户端每条请求都带 tools）。
func TestIdentityGateIgnoresTools(t *testing.T) {
	req := chatReq(userMsg("你是谁"))
	req.Tools = []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather"}}}
	if _, ok := identityGate(req); !ok {
		t.Fatal("声明 tools 时身份问句仍应命中")
	}
}

// responses 转过来的 input_text 块也要能进门（V3 曾因此漏检）。
func TestIdentityGateInputTextParts(t *testing.T) {
	req := chatReq(ChatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"input_text","text":"你是谁"}]`)})
	if _, ok := identityGate(req); !ok {
		t.Fatal("input_text 块的身份问句应命中")
	}
}

// 底座自述必须被识别（本站真机实测的泄漏方向）。
func TestBaseModelLeakDetection(t *testing.T) {
	leaks := []string{
		"我是 Codex，一个基于 GPT-5 的 AI 编程与协作代理。",
		"I am Codex, powered by GPT-5.",
		"我是本服务的通用助手，由 OpenAI 打造。",
		"我是由 OpenAI 训练的大语言模型。",
	}
	for _, s := range leaks {
		if !hasBaseModelLeak(s) {
			t.Fatalf("漏检底座自述：%q", s)
		}
	}
	clean := []string{
		"我是本服务的通用助手。",
		"牛顿第二定律是 F=ma。",
		"Codex 和 Claude 有什么区别？",
	}
	for _, s := range clean {
		if hasBaseModelLeak(s) {
			t.Fatalf("误伤正常文本：%q", s)
		}
	}
}

// 改写规则：干净回答原样；脏回答清洗；全脏回退身份答句。
func TestGuardIdentityAnswer(t *testing.T) {
	s := &Server{}
	if out, action := s.guardIdentityAnswer("你是谁", "我是本服务的通用助手。"); out != "我是本服务的通用助手。" || action != "passthrough" {
		t.Fatalf("干净回答应原样返回，got %q/%s", out, action)
	}
	if out, action := s.guardIdentityAnswer("", "我是 Codex。"); out != "我是 Codex。" || action != "passthrough" {
		t.Fatalf("非身份话题应原样返回，got %q/%s", out, action)
	}
	out, action := s.guardIdentityAnswer("你是谁", "我是 Codex，基于 GPT-5。\n\n牛顿第二定律是 F=ma。")
	if action != "sanitized" || strings.Contains(out, "Codex") || !strings.Contains(out, "F=ma") {
		t.Fatalf("应删掉自述段保留答案，got %q/%s", out, action)
	}
	out, action = s.guardIdentityAnswer("你是谁", "我是 Codex，基于 GPT-5。")
	if action != "identity" || out != defaultIdentityAnswer {
		t.Fatalf("全脏应回退默认答句，got %q/%s", out, action)
	}
}
