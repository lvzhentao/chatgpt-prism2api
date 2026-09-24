package prism

import (
	"testing"

	"prism-2api/internal/adapter"
	"prism-2api/internal/failclass"
)

// 上游把「模型名不在账号清单里」这类请求过错写成 HTTP 200 + 内联报错，
// 状态码只出现在文案里（"(400 Bad Request)"）。抽不出来就会被 failclass 当 server 类，
// 一个错模型名换整整 2 分钟的账号级冷却。
func TestStartFailedErrorKeepsUpstreamStatus(t *testing.T) {
	cases := []struct {
		msg      string
		wantCode int
		wantRe   bool // 是否值得换沙箱重试
	}{
		{"Error while processing conversation (400 Bad Request). Please submit prompt again.", 400, false},
		{"Error while processing conversation (500 Internal Server Error). Please submit prompt again.", 500, true},
		{"Please submit prompt again", 0, true},
	}
	for _, c := range cases {
		err := startFailedError(c.msg)
		if got := err.StatusCode(); got != c.wantCode {
			t.Errorf("%q: status=%d want %d", c.msg, got, c.wantCode)
		}
		if got := err.retryable(); got != c.wantRe {
			t.Errorf("%q: retryable=%v want %v", c.msg, got, c.wantRe)
		}
		if res := failclass.Classify(err); res.Cool != c.wantRe {
			t.Errorf("%q: Classify=%s cool=%v want cool=%v", c.msg, res.Class, res.Cool, c.wantRe)
		}
	}
}

// 请求过错必须原样报给客户端 400，而不是被抹成 502。
func TestStartFailedErrorClassifiesRequestFaultAsBadRequest(t *testing.T) {
	res := failclass.Classify(startFailedError(
		"Error while processing conversation (400 Bad Request). Please submit prompt again."))
	if res.Class != failclass.BadRequest {
		t.Fatalf("class=%s want %s", res.Class, failclass.BadRequest)
	}
	if res.HTTPStatus != 400 || res.Switch {
		t.Fatalf("http=%d switch=%v want 400/false", res.HTTPStatus, res.Switch)
	}
}

func TestModelNameAndEffortHandleLevelSuffix(t *testing.T) {
	cases := []struct {
		model     string
		thinking  string
		wantModel string
		wantLevel string
	}{
		{"gpt-5.6-sol-high", "", "gpt-5.6-sol", "high"},
		{"gpt-5.6-sol-low", "", "gpt-5.6-sol", "low"},
		{"gpt-6-astra-xhigh", "", "gpt-6-astra", "xhigh"},
		{"gpt-5.6-sol-xhigh", "", "gpt-5.6-sol", "xhigh"},
		{"gpt-5.6-sol-max", "", "gpt-5.6-sol", "xhigh"},
		{"gpt-5.6-sol-xhigh", "max", "gpt-5.6-sol", "xhigh"},
		// 客户端显式档位优先，但名字无论如何都要剥干净（否则上游 400）
		{"gpt-5.6-sol-high", "low", "gpt-5.6-sol", "low"},
		{"gpt-5.6-terra", "", "gpt-5.6-terra", "medium"},
		{"", "", DefaultModel, "medium"},
		// 真机：别名原样发上去一律 400（'sol' → "Error while processing conversation"），
		// 必须换成 ServerModelName 再发。
		{"sol", "", "gpt-5.6-sol", "medium"},
		{"default", "", "gpt-5.6-sol", "medium"},
		{"free", "", "gpt-5.6-terra", "medium"},
		{"astra", "", "gpt-6-astra", "medium"},
		{"sol-high", "", "gpt-5.6-sol", "high"},
		// 清单外的名字兜底默认模型：原样透传上游必回 400（2026-09-19 线上实锤，
		// sentinel 修复前被 403 掩盖），客户端拼错名字不该整轮报废。
		{"gpt-5.6-luna", "", "gpt-5.6-sol", "medium"},
		{"gpt-5", "", "gpt-5.6-sol", "medium"},
	}
	for _, c := range cases {
		nr := &adapter.NativeRequest{Model: c.model, Thinking: c.thinking}
		if got := modelName(nr); got != c.wantModel {
			t.Errorf("modelName(%q) = %q, want %q", c.model, got, c.wantModel)
		}
		if got := reasoningEffort(nr); got != c.wantLevel {
			t.Errorf("reasoningEffort(%q, %q) = %q, want %q", c.model, c.thinking, got, c.wantLevel)
		}
	}
}
