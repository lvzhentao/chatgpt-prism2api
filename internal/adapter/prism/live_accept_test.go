package prism

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

// 一次性真机验收（PRISM_LIVE=1）：走真实 Stream() 全链路，验证
// 「图片 base64 内联 → 模型还原 → view_image」与「文本附件正文内联」都真的可用。
// 长期验收在 scripts/smoke.py 的 LM 层（打部署好的网关）。
func TestLiveAttachmentAcceptance(t *testing.T) {
	if os.Getenv("PRISM_LIVE") == "" {
		t.Skip("PRISM_LIVE not set")
	}
	raw, err := os.ReadFile("../../../cookies/prism.openai.com_16-09-2026.json")
	if err != nil {
		t.Skipf("本地账号 cookie 不可用（%v）；真机验收走 scripts/smoke.py 的 LM 层", err)
	}
	c := newHTTPClient(adapter.ClientConfig{
		BaseURL:       "https://prism.openai.com",
		TokenProvider: func() (string, error) { return string(raw), nil },
	})
	ctx := context.Background()
	if _, err := c.loadCredential(); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureSession(ctx); err != nil {
		t.Fatal(err)
	}

	// 左红/中绿/右蓝三竖带：内容指纹，模型猜不出来。
	img := image.NewRGBA(image.Rect(0, 0, 90, 30))
	for y := range 30 {
		for x := range 90 {
			switch x * 3 / 90 {
			case 0:
				img.Set(x, y, color.RGBA{220, 20, 20, 255})
			case 1:
				img.Set(x, y, color.RGBA{20, 200, 20, 255})
			default:
				img.Set(x, y, color.RGBA{20, 20, 220, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	token := "PRISM-FILE-7391"
	cases := []struct {
		label  string
		prompt string
		files  []adapter.File
		want   []string
	}{
		{
			label:  "图片（三色竖带）",
			prompt: "图片里从左到右的颜色顺序是什么？只回复三个颜色词。",
			files:  []adapter.File{{Name: "bands.png", Mime: "image/png", Data: buf.Bytes()}},
			want:   []string{"红", "绿", "蓝"},
		},
		{
			label:  "文本附件",
			prompt: "附件里的验证口令是什么？只回复口令。",
			files: []adapter.File{{
				Name: "notes.txt", Mime: "text/plain", Data: []byte("验证口令：" + token),
				Text: "[Attached document: notes.txt media=text/plain]\n验证口令：" + token,
			}},
			want: []string{token},
		},
	}
	for _, tc := range cases {
		nr := &adapter.NativeRequest{
			Model:    DefaultModel,
			Messages: []adapter.ChatMessage{{Role: "user", Content: tc.prompt, Files: tc.files}},
		}
		var out strings.Builder
		if err := c.Stream(ctx, nr, func(ev adapter.Event) bool {
			out.WriteString(ev.Text)
			return true
		}); err != nil {
			t.Errorf("[%s] Stream err: %v", tc.label, err)
			continue
		}
		text := strings.TrimSpace(out.String())
		ok := true
		for _, w := range tc.want {
			if !strings.Contains(text, w) {
				ok = false
			}
		}
		status := "PASS"
		if !ok {
			status = "FAIL"
			t.Errorf("[%s] 期望包含 %v，实际: %s", tc.label, tc.want, truncate(text, 300))
		}
		fmt.Printf("[%s] %s → %s\n", status, tc.label, truncate(text, 200))
	}
}
