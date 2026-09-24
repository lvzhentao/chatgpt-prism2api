package prism

import (
	"context"
	"os"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// 关键问题：start 到底需不需要那 6 跳预热链？
// 若不需要（或只需其中一跳），就能跳过当前劣化的 backend 路由。
//
//	PRISM_LIVE_COOKIE_FILE=/tmp/prism-live-cookie.txt \
//	  go test -count=1 -run TestProbeStartWithoutSandbox -v -timeout 25m ./internal/adapter/prism/
func TestProbeStartWithoutSandbox(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
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

	ask := "回一个字：好"
	arms := []struct {
		name string
		mut  func(md map[string]any)
	}{
		{"A-无沙箱字段", func(md map[string]any) { delete(md, "sandbox_url"); delete(md, "sandbox_token") }},
		{"B-假沙箱token", func(md map[string]any) { md["sandbox_token"] = "gAAAAABfakefakefake" }},
		{"C-空沙箱url", func(md map[string]any) { md["sandbox_url"] = "" }},
	}

	for _, arm := range arms {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			body, err := probeBodyWithSandbox(ctx, c, projectID, ask)
			if err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			arm.mut(body["metadata"].(map[string]any))
			start := time.Now()
			callCtx, cancel := context.WithTimeout(ctx, 100*time.Second)
			defer cancel()
			resp, err := c.request(callCtx, "POST", c.origin()+PathChat, body, nil)
			el := time.Since(start)
			if err != nil {
				t.Logf("[%s] %.1fs 传输失败/超时: %v", arm.name, el.Seconds(), err)
				return
			}
			v := resp.json()
			t.Logf("[%s] %.1fs HTTP %d status=%q turn_state=%v resp.status=%q msg=%q",
				arm.name, el.Seconds(), resp.status, stringAt(v, "status"),
				len(rawAt(v, "turn_state")) > 0, stringAt(mapAt(v, "response"), "status"),
				truncate(inlineStartError(v), 120))
		})
	}
}

// probeBodyWithSandbox 自建一份「带真实沙箱凭据」的 start 请求体：探针先拿到正常路径的
// baseline，再逐臂删/改沙箱字段做对比，所以这里必须真的走一遍预热链。
func probeBodyWithSandbox(ctx context.Context, c *httpClient, projectID, prompt string) (map[string]any, error) {
	sandbox, err := c.ensureSandbox(ctx, projectID)
	if err != nil {
		return nil, err
	}
	conv := "cdx1_" + uuid4()
	if err := c.registerConversation(ctx, projectID, conv); err != nil {
		return nil, err
	}
	nr := &adapter.NativeRequest{
		Model:    "gpt-5.6-terra",
		Messages: []adapter.ChatMessage{{Role: "user", Content: prompt}},
	}
	sandboxURL := c.origin() + "/s/sandboxes/proxy/"
	return map[string]any{
		"input": buildInput(nr),
		"metadata": map[string]any{
			"projectId":             projectID,
			"userId":                c.accountID(),
			"model":                 modelName(nr),
			"reasoning_effort":      reasoningEffort(nr),
			"frontend_origin":       c.origin(),
			"sandbox_url":           sandboxURL,
			"sandbox_token":         sandbox,
			"codex_listen_snapshot": listenSnapshot(c.accountID(), projectID, conv, sandboxURL, sandbox),
		},
		"conversationId": conv,
	}, nil
}
