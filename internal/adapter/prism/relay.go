package prism

import (
	"bufio"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"prism-2api/internal/adapter"
	"strings"
	"sync/atomic"
	"time"
)

// ============================================================
// 浏览器栈代发（终局通道，2026-09-21 定稿）：
//
// 上游把 sentinel 验证扩到全部 API 面（start/沙箱/projects/status 全要
// 一次性票+完整浏览器上下文，服务端拼装形态被系统性淘汰——四轮追补全被
// 新扩面挡住的教训）。对话 turn 整体交给 login sidecar 的常驻浏览器栈：
// ctx.request 发 start（自动带全套 cookie/指纹）+ mint 票，同栈轮询到完成，
// 一次调用返回全文。护栏/追捞/打字机等 api 层逻辑全部不变（正文照常走
// Event 流）。吞吐顶 = sidecar mint 产票产能（首版 ~100-180 RPM），
// WEB2API_BROWSER_RELAY=off 回退直连通道。
// ============================================================

var (
	relayTurnRR   atomic.Uint64
	sidecarPoolRR atomic.Uint64
)

// sidecarBaseURLs 铸票/代发用的 sidecar 池：PRISM_LOGIN_SIDECAR_URL 为主，
// 逗号分隔可列多个（prism-login,prism-login2...），轮转分流铸票产能。
func sidecarBaseURLs() []string {
	main := SidecarURL()
	if main == "" {
		return nil
	}
	pool := []string{main}
	if v := strings.TrimSpace(os.Getenv("PRISM_LOGIN_SIDECAR_POOL")); v != "" {
		for _, u := range strings.Split(v, ",") {
			u = strings.TrimRight(strings.TrimSpace(u), "/")
			if u != "" && u != main {
				pool = append(pool, u)
			}
		}
	}
	return pool
}

func pickSidecar() string {
	pool := sidecarBaseURLs()
	if len(pool) <= 1 {
		return SidecarURL()
	}
	return pool[int(sidecarPoolRR.Add(1))%len(pool)]
}

// browserRelayOn 浏览器代发开关（默认开；显式 off 时回退直连）。
func browserRelayOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_BROWSER_RELAY")))
	return v != "0" && v != "false" && v != "off" && sentinelEnabled()
}

// relayChatTurn 把一轮对话完整交给 sidecar 浏览器栈，返回正文与思考摘要。
func relayChatTurn(ctx context.Context, nr *adapter.NativeRequest) (text, reasoning string, err error) {
	body, merr := json.Marshal(map[string]any{
		"action":      "chat_turn",
		"input":       buildInput(nr),
		"model":       modelName(nr),
		"effort":      reasoningEffort(nr),
		"state_index": int(relayTurnRR.Add(1) % 8),
	})
	if merr != nil {
		return "", "", merr
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, pickSidecar()+"/login", bytes.NewReader(body))
	if rerr != nil {
		return "", "", rerr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	// turn 是长活（30~120s）：连接超时放宽到 4 分钟（与轮询上限+浏览器流程匹配）。
	cl := &http.Client{Timeout: 4 * time.Minute}
	resp, derr := cl.Do(req)
	if derr != nil {
		return "", "", fmt.Errorf("prism: relay sidecar: %w", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", "", fmt.Errorf("prism: relay sidecar status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	var errMsg string
	for scanner.Scan() {
		var ev struct {
			Event     string `json:"event"`
			Text      string `json:"text"`
			Reasoning string `json:"reasoning"`
			Message   string `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Event {
		case "done":
			return ev.Text, ev.Reasoning, nil
		case "error":
			errMsg = ev.Message
		}
	}
	if errMsg != "" {
		return "", "", fmt.Errorf("prism: relay: %s", truncate(errMsg, 200))
	}
	return "", "", fmt.Errorf("prism: relay produced no result")
}

// relayStream 浏览器代发版 Stream：拿全文后按 Event 语义回放（含 <tool_call>
// 抽取，与直连通道一致），api 层的护栏/追捞/打字机全部照常工作。
func (c *httpClient) relayStream(ctx context.Context, nr *adapter.NativeRequest, emit func(adapter.Event) bool) error {
	text, reasoning, err := relayChatTurn(ctx, nr)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if len(nr.Tools) > 0 {
		if emulated, cleaned := extractToolCallsFromText(text); len(emulated) > 0 {
			text = cleaned
			// 工具调用优先回放；正文（若有）随后。
			if text != "" && reasoning != "" {
				if !emit(adapter.Event{Thinking: &adapter.Thinking{Text: reasoning, IsLastThinkingChunk: true}}) {
					return nil
				}
			}
			for i := range emulated {
				tc := emulated[i]
				if !emit(adapter.Event{ToolCall: &tc}) {
					return nil
				}
			}
			if text != "" && !emit(adapter.Event{Text: text}) {
				return nil
			}
			emit(adapter.Event{Ended: true})
			return nil
		}
	}
	if reasoning != "" {
		if !emit(adapter.Event{Thinking: &adapter.Thinking{Text: reasoning, IsLastThinkingChunk: true}}) {
			return nil
		}
	}
	if text != "" && !emit(adapter.Event{Text: text}) {
		return nil
	}
	emit(adapter.Event{Ended: true})
	return nil
}
