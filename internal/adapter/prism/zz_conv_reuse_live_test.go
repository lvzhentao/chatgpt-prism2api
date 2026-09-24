package prism

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// intAt 从 map 里取整数（json 数字解成 float64）。
func intAt(v map[string]any, key string) int {
	f, _ := v[key].(float64)
	return int(f)
}

// 会话复用真机实验（P1-3）：同一 conversationId + previousResponseId 连续两轮，
// 上游是否真的带着上一轮记忆作答。是 → 复用可行（成本/缓存/放大都有解）；
// 否 → 维持"每轮新会话+历史折叠"现状。
//
//	PRISM_LIVE_COOKIE_FILE=/tmp/prism-live-cookie.txt \
//	  go test -count=1 -run TestProbeConversationReuse -v -timeout 20m ./internal/adapter/prism/
func TestProbeConversationReuse(t *testing.T) {
	cookieFile := os.Getenv("PRISM_LIVE_COOKIE_FILE")
	if cookieFile == "" {
		t.Skip("设置 PRISM_LIVE_COOKIE_FILE 才跑真机探针")
	}
	raw, err := os.ReadFile(cookieFile)
	if err != nil {
		t.Fatalf("读 cookie 失败: %v", err)
	}
	c := newHTTPClient(adapter.ClientConfig{
		BaseURL:       "https://prism.openai.com",
		TokenProvider: func() (string, error) { return string(raw), nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	if _, err := c.loadCredential(); err != nil {
		t.Fatalf("loadCredential: %v", err)
	}
	if err := c.ensureSession(ctx); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	projectID, err := c.ensureProject(ctx)
	if err != nil {
		t.Fatalf("ensureProject: %v", err)
	}
	sandbox, err := c.ensureSandbox(ctx, projectID)
	if err != nil {
		t.Fatalf("ensureSandbox: %v", err)
	}

	runRound := func(conv, prompt, prevRespID, snapshotOverride string) (text, respID string, err error) {
		nr := &adapter.NativeRequest{
			Model:    "gpt-5.6-sol",
			Messages: []adapter.ChatMessage{{Role: "user", Content: prompt}},
		}
		sandboxURL := c.origin() + "/s/sandboxes/proxy/"
		snap := snapshotOverride
		if snap == "" {
			snap = listenSnapshot(c.accountID(), projectID, conv, sandboxURL, sandbox)
		}
		body := map[string]any{
			"input": buildInput(nr),
			"metadata": map[string]any{
				"projectId":             projectID,
				"userId":                c.accountID(),
				"model":                 modelName(nr),
				"reasoning_effort":      reasoningEffort(nr),
				"frontend_origin":       c.origin(),
				"sandbox_url":           sandboxURL,
				"sandbox_token":         sandbox,
				"codex_listen_snapshot": snap,
			},
			"conversationId": conv,
		}
		if prevRespID != "" {
			body["previousResponseId"] = prevRespID
		}
		start := time.Now()
		resp, err := c.request(ctx, "POST", c.origin()+PathChat, body, nil)
		if err != nil {
			return "", "", err
		}
		v := resp.json()
		ts := rawAt(v, "turn_state")
		reqID := stringAt(v, "request_id")
		if len(ts) == 0 {
			// 无 turn_state 的 completed = sandbox_reconnecting 类，不能轮询（真机已知）
			return "", "", &upstreamError{Status: resp.status, Msg: "no turn_state: " + truncate(inlineStartError(v), 160)}
		}
		t.Logf("  start %.1fs request_id=%s", time.Since(start).Seconds(), reqID)
		// 手动轮询到 completed
		for attempt := 1; attempt <= 120; attempt++ {
			pollBody := map[string]any{"request_id": reqID, "turn_state": ts}
			pr, err := c.request(ctx, "POST", c.origin()+PathStatus, pollBody, nil)
			if err != nil {
				return "", "", err
			}
			pv := pr.json()
			if nts := rawAt(pv, "turn_state"); len(nts) > 0 && string(nts) != "null" {
				ts = nts
			}
			switch stringAt(pv, "status") {
			case "completed":
				payload := mapAt(mapAt(pv, "response"), "payload")
				return extractText(payload), stringAt(payload, "id"), nil
			case "failed", "error":
				return "", "", &upstreamError{Status: pr.status, Msg: "poll failed: " + truncate(string(pr.body), 200)}
			}
			time.Sleep(pollInterval(attempt))
		}
		return "", "", &upstreamError{Msg: "poll timeout"}
	}

	const secret = "4173"
	conv := "cdx1_" + uuid4()
	if err := c.registerConversation(ctx, projectID, conv); err != nil {
		t.Fatalf("registerConversation: %v", err)
	}

	t.Log("round1: 写入数字 4173")
	text1, resp1, err := runRound(conv, "记住这个数字：4173。只回复一个字：好", "", "")
	if err != nil {
		t.Fatalf("round1: %v", err)
	}
	t.Logf("round1 完成: text=%q payload_id=%s", truncate(text1, 60), resp1)
	if resp1 == "" {
		t.Fatal("round1 completed 但 payload.id 为空——没有 previousResponseId 可用")
	}

	t.Log("round2: 同 conversationId + previousResponseId（snapshot 全 null），问回数字")
	text2, _, err := runRound(conv, "刚才让你记的数字是多少？只回答数字本身", resp1, "")
	if err != nil {
		t.Fatalf("round2: %v", err)
	}
	t.Logf("round2 完成: text=%q", truncate(text2, 80))
	reused := strings.Contains(text2, secret)

	// round2b：用 runtime/debug 拿真实 snapshot 字段（codex_session_id/last_turn_id/transcript_cursor）
	// 再续一轮——排除"复用失败只是 snapshot 全 null"的混淆变量。
	dbg, derr := c.request(ctx, "GET", c.origin()+"/api/codex/runtime/debug?conversation_id="+conv, nil, nil)
	if derr == nil && dbg.status < 300 {
		if snap := mapAt(dbg.json(), "snapshot"); snap != nil {
			csid := stringAt(snap, "codex_session_id")
			ltid := stringAt(snap, "last_turn_id")
			cursor := intAt(snap, "transcript_cursor")
			t.Logf("runtime/debug snapshot: codex_session_id=%s last_turn_id=%s cursor=%d", csid, ltid, cursor)
			if csid != "" || ltid != "" {
				now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
				snap2 := map[string]any{
					"user_id": c.accountID(), "project_id": projectID, "conversation_id": conv,
					"sandbox_url": c.origin() + "/s/sandboxes/proxy/", "sandbox_token": sandbox,
					"workspace_session_id": strings.TrimPrefix(conv, "cdx1_"),
					"codex_session_id":     csid, "last_turn_id": ltid,
					"transcript_cursor": cursor, "created_at": now, "updated_at": now,
				}
				rawSnap, _ := json.Marshal(snap2)
				t.Log("round2b: 同会话 + previousResponseId + 真实 snapshot 字段")
				text2b, _, err := runRound(conv, "刚才让你记的数字是多少？只回答数字本身", resp1, string(rawSnap))
				if err != nil {
					t.Logf("round2b 失败: %v", err)
				} else {
					t.Logf("round2b 完成: text=%q 命中=%v", truncate(text2b, 80), strings.Contains(text2b, secret))
					if strings.Contains(text2b, secret) {
						reused = true
					}
				}
			}
		}
	} else {
		t.Logf("runtime/debug 不可用，跳过 round2b: err=%v status=%d", derr, dbg.status)
	}

	t.Log("round3 对照: 新会话不带 previousResponseId，同问")
	conv3 := "cdx1_" + uuid4()
	if err := c.registerConversation(ctx, projectID, conv3); err != nil {
		t.Fatalf("registerConversation round3: %v", err)
	}
	text3, _, err := runRound(conv3, "刚才让你记的数字是多少？只回答数字本身", "", "")
	if err != nil {
		t.Fatalf("round3: %v", err)
	}
	t.Logf("round3 完成: text=%q", truncate(text3, 80))
	controlHas := strings.Contains(text3, secret)

	t.Logf("== 结论: 复用轮命中=%v 对照轮命中=%v ==", reused, controlHas)
	if reused && !controlHas {
		t.Log("✅ 会话复用可行：上游认了 previousResponseId 且带记忆")
	} else if reused && controlHas {
		t.Log("⚠️ 两轮都答对——模型可能在蒙（数字简单），证据弱")
	} else {
		t.Log("❌ 复用不可行或不带记忆")
	}
}
