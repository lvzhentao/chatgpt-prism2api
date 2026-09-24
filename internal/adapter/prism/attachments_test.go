package prism

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"

	"prism-2api/internal/adapter"
)

// makePNG 生成一张带噪声的测试图（噪声让 PNG 不可压缩，用来压大小相关分支）。
func makePNG(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewPCG(1, 2))
	for y := range h {
		for x := range w {
			n := 0
			if noisy {
				n = rnd.IntN(40) - 20
			}
			clamp := func(v int) uint8 {
				if v < 0 {
					return 0
				}
				if v > 255 {
					return 255
				}
				return uint8(v)
			}
			switch x * 3 / w {
			case 0:
				img.Set(x, y, color.RGBA{clamp(220 + n), clamp(30 + n), clamp(30 + n), 255})
			case 1:
				img.Set(x, y, color.RGBA{clamp(30 + n), clamp(200 + n), clamp(30 + n), 255})
			default:
				img.Set(x, y, color.RGBA{clamp(30 + n), clamp(30 + n), clamp(220 + n), 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 上游没有图片通道（真机：input_image 被拒、沙箱看不到项目文件），
// 唯一走通的路是把 base64 塞进提示词 + 让模型自己还原成工作区文件再 view_image。
// 这条契约一旦丢（少了还原命令或 view_image 指引），图片就等于没送上去。
func TestRenderImageInlinesBase64AndInstructions(t *testing.T) {
	data := makePNG(t, 90, 30, false)
	block, cost := renderAttachment(adapter.File{Name: "shot.png", Mime: "image/png", Data: data}, 0, inlineB64Budget)

	want := base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(block, want) {
		t.Fatalf("图片块必须包含 base64（模型只能从这里拿到图像）")
	}
	if cost != len(want) {
		t.Fatalf("cost = %d, want %d（预算按 base64 长度扣）", cost, len(want))
	}
	for _, must := range []string{"base64 -d", "prism-uploads/", "view_image", "shot.png"} {
		if !strings.Contains(block, must) {
			t.Fatalf("图片块缺少 %q：\n%s", must, block)
		}
	}
	if !strings.Contains(block, "不要把 base64 复述") {
		t.Fatalf("要明确禁止模型复述 base64（否则正文会被 base64 淹没）")
	}
}

// 超预算的图必须先缩再内联，且缩完不能超预算——真机 219k 字符的请求上游直接 502。
func TestRenderImageShrinksToBudget(t *testing.T) {
	big := makePNG(t, 900, 600, true)
	if len(base64.StdEncoding.EncodeToString(big)) <= inlineB64Budget {
		t.Fatalf("测试图不够大（%d 字节），无法覆盖缩图分支", len(big))
	}
	block, cost := renderAttachment(adapter.File{Name: "photo.png", Mime: "image/png", Data: big}, 0, inlineB64Budget)
	if cost == 0 || cost > inlineB64Budget {
		t.Fatalf("cost = %d, want 0 < cost <= %d", cost, inlineB64Budget)
	}
	if !strings.Contains(block, "为传输已缩为") {
		t.Fatalf("缩过图的块要说明已缩小（否则用户不知道精度被降过）:\n%s", block[:400])
	}
	// 内联的确实是可解码的 JPEG
	i := strings.Index(block, "printf '%s' '")
	j := strings.Index(block[i:], "' | base64 -d")
	raw, err := base64.StdEncoding.DecodeString(block[i+len("printf '%s' '") : i+j])
	if err != nil {
		t.Fatalf("内联的不是合法 base64: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("缩图结果不是可解码 JPEG: %v", err)
	}
}

// 预算不足时不能静默丢图：要留一句说明，让模型/用户知道发生了什么。
func TestRenderImageOverBudgetKeepsNotice(t *testing.T) {
	big := makePNG(t, 900, 600, true)
	block, cost := renderAttachment(adapter.File{Name: "photo.png", Mime: "image/png", Data: big}, 0, 1024)
	if cost != 0 {
		t.Fatalf("预算 1024 时不该内联，cost = %d", cost)
	}
	if !strings.Contains(block, "未载入") {
		t.Fatalf("超预算要走说明分支，got %q", block)
	}
	if strings.Contains(block, "base64 -d") {
		t.Fatalf("预算不足时不能还是内联，got %q", block[:200])
	}
}

// 文本/PDF 附件直接用 api 层抽好的正文（上游没有二进制通道，这是它们唯一能工作的形态）。
func TestRenderDocumentInlinesText(t *testing.T) {
	block, cost := renderAttachment(adapter.File{
		Name: "notes.txt", Mime: "text/plain", Data: []byte("hi"),
		Text: "[Attached document: notes.txt media=text/plain bytes=2]\n验证口令 PRISM-FILE-7391",
	}, 0, inlineB64Budget)
	if cost != 0 {
		t.Fatalf("文本附件不占内联预算，cost = %d", cost)
	}
	if !strings.Contains(block, "PRISM-FILE-7391") {
		t.Fatalf("正文没进块：%s", block)
	}
}

// 认不出的二进制至少要说清楚，别让模型以为"用户没发附件"。
func TestRenderBinaryFallsBackToNotice(t *testing.T) {
	block, cost := renderAttachment(adapter.File{Name: "a.zip", Mime: "application/zip", Data: []byte("PK\x03\x04")}, 0, inlineB64Budget)
	if cost != 0 || !strings.Contains(block, "a.zip") || !strings.Contains(block, "无法直接送入模型") {
		t.Fatalf("二进制兜底块不对: %q", block)
	}
}

// 多图共享总预算：前面的图吃掉预算后，后面的图必须退化成标记，而不是硬塞。
func TestAttachmentBlocksRespectsTotalBudget(t *testing.T) {
	budget := 4096
	a := makePNG(t, 900, 600, true)
	blocks := attachmentBlocks([]adapter.File{
		{Name: "a.png", Mime: "image/png", Data: a},
		{Name: "b.png", Mime: "image/png", Data: a},
	}, &budget)
	if len(blocks) != 2 {
		t.Fatalf("每个附件都要有一块，got %d", len(blocks))
	}
	if budget < 0 {
		t.Fatalf("预算被超支：%d", budget)
	}
	if !strings.Contains(blocks[1], "未载入") {
		t.Fatalf("第二张图应因预算不足退化成说明，got %q", blocks[1][:min(200, len(blocks[1]))])
	}
}

// 图片-only 的请求没有文本，附件块仍必须落在本轮 user item 里。
// 回归保护：老逻辑按"文本非空"认本轮请求，会把图当历史丢掉（模型答"我看不到图片"）。
func TestBuildInputKeepsImageOnlyTurn(t *testing.T) {
	withoutInjectedPrompt(t)
	png := adapter.File{Name: "cat.png", Mime: "image/png", Data: []byte("png-bytes")}
	nr := &adapter.NativeRequest{Messages: []adapter.ChatMessage{
		{Role: "user", Content: "之前的问题"},
		{Role: "assistant", Content: "之前的回答"},
		{Role: "user", Files: []adapter.File{png}}, // 无文本
	}}
	items := buildInput(nr)
	last := items[len(items)-1]
	if itemRole(t, last) != "user" {
		t.Fatalf("最后一条必须是 user，got %s", dump(items))
	}
	if got := itemText(t, last); !strings.Contains(got, base64.StdEncoding.EncodeToString(png.Data)) {
		t.Fatalf("图片-only 请求必须把图内联进本轮 user 消息，got %q", truncate(got, 200))
	}
	// 历史里的同一张图不该被重复内联（预算只够一次）。
	if hist := itemText(t, items[0]); strings.Contains(hist, base64.StdEncoding.EncodeToString(png.Data)) {
		t.Fatalf("历史消息不该重复内联同一张图：%q", truncate(hist, 200))
	}
}

// 带 tools 时上游忽略 system，附件块必须并进同一条 user 消息。
func TestBuildInputToolsPathCarriesAttachment(t *testing.T) {
	withoutInjectedPrompt(t)
	png := adapter.File{Name: "cat.png", Mime: "image/png", Data: []byte("png-bytes")}
	nr := &adapter.NativeRequest{
		Messages: []adapter.ChatMessage{{Role: "user", Content: "图里是啥", Files: []adapter.File{png}}},
		Tools:    []adapter.ToolDef{{Name: "get_weather", Description: "查天气"}},
	}
	items := buildInput(nr)
	if len(items) != 1 || itemRole(t, items[0]) != "user" {
		t.Fatalf("带 tools 时必须只有一条 user item，got %s", dump(items))
	}
	got := itemText(t, items[0])
	if !strings.Contains(got, base64.StdEncoding.EncodeToString(png.Data)) || !strings.Contains(got, "view_image") {
		t.Fatalf("附件没并进 user 消息：%q", truncate(got, 300))
	}
}

// 文件名的会进 shell 命令，路径分隔符/引号必须清掉。
func TestAttachmentNameSanitizes(t *testing.T) {
	cases := []struct{ in, contains, absent string }{
		{"../../etc/passwd", "passwd.png", ".."},
		{"C:\\Users\\me\\shot.png", "shot.png", `\`},
		{"it's.png", "it_s.png", "'"},
		{"", "image-1.png", ""},
	}
	for _, c := range cases {
		got := attachmentName(c.in, "image/png", 0)
		if !strings.Contains(got, c.contains) {
			t.Errorf("attachmentName(%q) = %q, want contains %q", c.in, got, c.contains)
		}
		if c.absent != "" && strings.Contains(got, c.absent) {
			t.Errorf("attachmentName(%q) = %q, want without %q", c.in, got, c.absent)
		}
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "'") {
			t.Errorf("attachmentName(%q) = %q，不能带路径分隔符或单引号", c.in, got)
		}
	}
}
