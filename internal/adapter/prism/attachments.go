package prism

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 解码用（注册 GIF 解码器）
	"image/jpeg"
	_ "image/png" // 解码用（注册 PNG 解码器）
	"net/http"
	"strings"

	"prism-2api/internal/adapter"
)

// ============================================================
// 附件（图片 / 文档）怎么送上去。
//
// 上游没有任何二进制/多模态入参（真机实测 2026-09-17）：
//   - 站点自己的路子是「上传成项目文件 + input 里给 project_path」，但那条路只在
//     **浏览器编辑器把文件写进项目文档**时才成立：会话工作区
//     （/codex_workspace/<conv>）只物化项目文件树里已有的文件。headless 上传的文件
//     进不了文件树，模型在工作区里看不到（实测 `ls prism-uploads` → No such file）。
//   - input_image 要求 "valid Prism storage URL"（data: / 公网 URL / file_id 全被拒）。
//   - 沙箱出网被代理拦（"Domain forbidden"），模型不能自己下载外部图片。
//
// 唯一走通的路（实测 ✅）：把内容当文本塞进提示词，让模型自己还原成工作区文件再
// 用 view_image 看——图片 base64 + 一条还原命令；文本/PDF 直接用 api 层抽好的正文。
// ============================================================

const (
	// inlineB64Budget 单张图片内联的 base64 字符上限。
	// 实测：59k 字符可用（约 4 分钟，慢）；219k 字符上游直接 502。
	inlineB64Budget = 48 << 10
	// inlineB64Total 一条请求里所有图片内联的总预算（多图按新消息优先分配）。
	inlineB64Total = 96 << 10
	// maxImageDim 缩图后的最长边（再大对识别没帮助，只是烧 token）。
	maxImageDim = 1400
)

// fileKey 是附件在请求内的标识：内容摘要（同名不同内容也能区分）。
func fileKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// renderAttachment 把一个附件渲染成提示词里的文本块。
// cost 是本块消耗的内联预算（字符数）：图片按 base64 长度计，文本/说明块为 0。
// budget 是**单张**图片可用的剩余预算（调用方按总预算分配）。
func renderAttachment(f adapter.File, seq, budget int) (block string, cost int) {
	switch {
	case strings.TrimSpace(f.Text) != "":
		return fmt.Sprintf("[文档附件 %d] %s（%s，%d 字节）正文如下：\n%s",
			seq+1, attachmentName(f.Name, f.Mime, seq), mimeOrGuess(f.Mime, f.Data), len(f.Data),
			strings.TrimSpace(f.Text)), 0
	case isImage(f.Mime):
		return renderImage(f, seq, budget)
	default:
		return fmt.Sprintf("[附件 %d] %s（%s，%d 字节）：二进制内容无法直接送入模型，可按需向用户索取文本版本。",
			seq+1, attachmentName(f.Name, f.Mime, seq), mimeOrGuess(f.Mime, f.Data), len(f.Data)), 0
	}
}

// renderImage 把图片渲染成「base64 + 还原命令 + 查看说明」的块。
func renderImage(f adapter.File, seq, budget int) (string, int) {
	name := attachmentName(f.Name, f.Mime, seq)
	if len(f.Data) == 0 {
		return fmt.Sprintf("[图片附件 %d] %s（%s）：内容为空，未载入。", seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
	}
	if budget <= 0 {
		return fmt.Sprintf("[图片附件 %d] %s（%s）：本轮内联预算已用尽，未载入；"+
			"如需查看请让用户单独重发这一张。", seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
	}
	limit := min(inlineB64Budget, budget)
	b64 := base64.StdEncoding.EncodeToString(f.Data)
	note := ""
	if len(b64) > limit {
		src, ok := decodeImage(f.Data)
		if !ok {
			return fmt.Sprintf("[图片附件 %d] %s（%s，%d 字节）：这个格式没有内置解码器（只支持 PNG/JPEG/GIF），"+
				"且原图超过内联上限，本轮无法把图片内容交给模型。请让用户转成 PNG/JPEG 后重发。",
				seq+1, name, mimeOrGuess(f.Mime, f.Data), len(f.Data)), 0
		}
		small, ok := encodeWithin(src, limit)
		if !ok {
			return fmt.Sprintf("[图片附件 %d] %s（%s）：图片过大，缩到内联上限后仍放不下，"+
				"本轮未载入；如需查看请让用户单独重发这一张。",
				seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
		}
		note = fmt.Sprintf("（原图 %d 字节，为传输已缩为 %d 字节）", len(f.Data), len(small))
		b64 = base64.StdEncoding.EncodeToString(small)
	}
	return fmt.Sprintf(`[图片附件 %d] %s（%s）%s
图片内容只在这段 base64 里，直接读 base64 看不到图像。请先在工作区还原成文件，再查看它：

mkdir -p prism-uploads && printf '%%s' '%s' | base64 -d > prism-uploads/%s

然后用 view_image 查看 prism-uploads/%s（该工具不可用时，用你手头能看图的工具），再回答用户。
不要把 base64 复述进回答。`, seq+1, name, mimeOrGuess(f.Mime, f.Data), note, b64, name, name), len(b64)
}

// attachmentBlocks 生成一条消息里全部附件的块（按出现顺序），并从 budget 里扣掉内联开销。
func attachmentBlocks(files []adapter.File, budget *int) []string {
	blocks := make([]string, 0, len(files))
	for i, f := range files {
		block, cost := renderAttachment(f, i, *budget)
		*budget -= cost
		blocks = append(blocks, block)
	}
	return blocks
}

// attachmentName 取一个安全的文件名（客户端常给空名或带路径的名字）。
func attachmentName(name, mime string, seq int) string {
	n := sanitizeFileName(name)
	if n == "" {
		kind := "file"
		if isImage(mime) {
			kind = "image"
		}
		n = fmt.Sprintf("%s-%d", kind, seq+1)
	}
	if !strings.Contains(n, ".") {
		n += mimeExtension(mime)
	}
	return n
}

// sanitizeFileName 去掉路径与控制字符，并限长（文件名会进 shell 命令与提示词）。
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(":*?\"<>|'`$&;()[]{}#!", r):
			return '_'
		}
		return r
	}, name)
	if runes := []rune(name); len(runes) > 80 {
		name = string(runes[:80])
	}
	return name
}

func isImage(mime string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/")
}

var mimeExtensions = map[string]string{
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/jpg":        ".jpg",
	"image/webp":       ".webp",
	"image/gif":        ".gif",
	"image/bmp":        ".bmp",
	"image/tiff":       ".tiff",
	"application/pdf":  ".pdf",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"application/json": ".json",
	"application/zip":  ".zip",
}

// mimeExtension 由 MIME 猜扩展名；猜不到就空（不改文件名）。
func mimeExtension(mime string) string {
	return mimeExtensions[strings.ToLower(strings.TrimSpace(mime))]
}

// mimeOrGuess 用 MIME，缺失时按内容嗅探（只取主类型，它要进提示词）。
func mimeOrGuess(mime string, data []byte) string {
	if m := strings.TrimSpace(mime); m != "" {
		return m
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
}

// ---------------------------------------------------------------- 缩图

// encodedLen 是 base64 编码后的长度（预算判断用）。
func encodedLen(n int) int {
	if n == 0 {
		return 0
	}
	return (n + 2) / 3 * 4
}

// decodeImage 解出图片（只认标准库带解码器的格式：PNG/JPEG/GIF）。
func decodeImage(data []byte) (image.Image, bool) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	if src.Bounds().Dx() == 0 || src.Bounds().Dy() == 0 {
		return nil, false
	}
	return src, true
}

// encodeWithin 把图片压到 base64 长度不超 budget 为止（先限边长，再降比例/质量）。
func encodeWithin(src image.Image, budgetChars int) ([]byte, bool) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxImageDim {
		scale := float64(maxImageDim) / float64(long)
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	}
	src = flatten(src)
	for _, q := range []int{82, 70, 55} {
		for _, factor := range []float64{1, 0.75, 0.55, 0.4, 0.28, 0.2} {
			tw, th := max(1, int(float64(w)*factor)), max(1, int(float64(h)*factor))
			var out bytes.Buffer
			if err := jpeg.Encode(&out, downscale(src, tw, th), &jpeg.Options{Quality: q}); err != nil {
				continue
			}
			if encodedLen(out.Len()) <= budgetChars {
				return out.Bytes(), true
			}
		}
	}
	return nil, false
}

// flatten 把透明像素合成到白底（JPEG 没有 alpha，透明区会变黑）。
func flatten(src image.Image) image.Image {
	if o, ok := src.(interface{ Opaque() bool }); ok && o.Opaque() {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// downscale 盒式平均缩放（无第三方依赖；缩文字截图比最近邻清楚）。
func downscale(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0, y1 := b.Min.Y+y*sh/h, b.Min.Y+(y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range w {
			x0, x1 := b.Min.X+x*sw/w, b.Min.X+(x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+cr, g+cg, bl+cb, a+ca, n+1
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return dst
}
