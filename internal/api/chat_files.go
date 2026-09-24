package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/emulation"
)

// ============================================================
// 附件（多模态输入）
//
// 网关只负责把各协议里的附件块归一成字节；真正的落地由适配器做
// （prism 走 /api/project-files/upload，见 internal/adapter/prism/files.go）。
//
// 认三种载荷形态：
//   - data: URL（base64 或百分号编码）
//   - 裸 base64（OpenAI 的 file_data 允许）
//   - http(s) 地址（服务端代拉，受下面的限额约束）
// ============================================================

const (
	// maxAttachmentBytes 单个附件上限；请求体本身另有 32MB 的读取闸（ioLimit）。
	maxAttachmentBytes = 20 << 20
	// maxAttachments 单个请求的附件数上限：每个附件都是一次上游上传，
	// 不设上限时一条请求就能把账号拖住，也把项目塞满。
	maxAttachments = 16
	// remoteFetchConcurrency 远端附件并行拉取的并发上限（T1.5）：
	// semaphore channel 限容，其余排队；单文件 attachmentFetchTimeout 与
	// maxAttachments 总量上限不变。
	remoteFetchConcurrency = 4
	// attachmentFetchTimeout 代拉远端附件的超时。
	attachmentFetchTimeout = 20 * time.Second
	// attachmentRedirects 代拉远端附件允许的重定向次数。
	attachmentRedirects = 3
)

// attachmentClient 只管代拉远端附件：限时限量、拒绝跨协议重定向。
var attachmentClient = &http.Client{
	Timeout: attachmentFetchTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= attachmentRedirects {
			return fmt.Errorf("stopped after %d redirects", attachmentRedirects)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to %q", req.URL.String())
		}
		return nil
	},
}

// extractedFile 是从 content parts 里抽出的附件。
type extractedFile struct {
	Name string // 客户端给的原始文件名，可能为空（由适配器补）
	Mime string // 可能为空
	Data []byte
	Text string // 文本类附件（text/*、PDF…）抽出的正文；图片为空
}

// textLike 判断这个附件要不要预抽文本：上游没有二进制通道，
// 文本/PDF 抽成正文最省事；图片走另一条路（见 prism attachments.go）。
func textLike(mime, name string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch {
	case strings.HasPrefix(m, "text/"):
		return true
	case m == "application/pdf", m == "application/json", m == "application/xml",
		m == "application/x-yaml", m == "application/yaml", m == "application/csv",
		m == "application/x-ndjson", m == "application/javascript":
		return true
	case m == "":
		// 没给 MIME 就按扩展名猜（客户端常常两个都不给）。
		switch strings.ToLower(path.Ext(name)) {
		case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".yaml", ".yml",
			".xml", ".html", ".htm", ".log", ".tex", ".bib", ".go", ".py", ".js", ".ts",
			".java", ".c", ".h", ".cpp", ".rs", ".sh", ".sql", ".toml", ".ini", ".pdf":
			return true
		}
	}
	return false
}

// extractFiles 从 content parts 里抽出全部附件（图片 / 文档 / 任意二进制）。
// 认三种块：
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}
//	{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,…"}}
//	{"type":"input_file","filename":"a.pdf","file_data":"…"}   // Responses
//
// 解析不了的块只记日志、不静默（模型看不到图时，日志是唯一线索）。
func extractFiles(ctx context.Context, raw json.RawMessage) []extractedFile {
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return nil
	}
	// T1.5：远端附件并行代拉。本地载荷（data:/裸 base64）仍同步解析；
	// 远端 URL 用 semaphore channel 限 4 并发、其余排队，单文件
	// attachmentFetchTimeout 与 maxAttachments 总量上限不变。
	// 任一远端失败只丢该附件；最终按 parts 原序合并，保证总量闸与
	// 日志语义和串行版一致。
	type slot struct {
		idx     int    // parts 下标（日志用）
		typ     string // parts 类型（日志用）
		name    string // 原始文件名（未 trim，日志用）
		payload string // 原始载荷（分类与 resolve 用，和串行版同一字符串）
		remote  bool   // 是否走远端代拉
		file    extractedFile
		err     error
	}
	var slots []slot
	for i, p := range parts {
		name, payload := "", ""
		switch {
		case p.ImageURL != nil && strings.TrimSpace(p.ImageURL.URL) != "":
			name, payload = "", p.ImageURL.URL
		case p.File != nil:
			name, payload = p.File.Filename, p.File.FileData
		default:
			name, payload = p.Filename, p.FileData
		}
		if strings.TrimSpace(payload) == "" {
			continue
		}
		s := slot{idx: i, typ: p.Type, name: name, payload: payload}
		if isRemoteURL(payload) {
			s.remote = true
		} else {
			// 本地载荷：与串行版同一调用，语义不动。
			s.file, s.err = resolveAttachment(ctx, strings.TrimSpace(name), payload)
		}
		slots = append(slots, s)
	}
	// 远端并发拉取：semaphore 限 4，其余排队；失败只记在该 slot 里。
	var wg sync.WaitGroup
	sem := make(chan struct{}, remoteFetchConcurrency)
	for i := range slots {
		if !slots[i].remote {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			name := strings.TrimSpace(slots[i].name)
			data, mime, err := fetchAttachment(ctx, slots[i].payload)
			if err != nil {
				slots[i].err = err
				return
			}
			if name == "" {
				name = urlBaseName(slots[i].payload)
			}
			slots[i].file = extractedFile{Name: name, Mime: mime, Data: data}
		}(i)
	}
	wg.Wait()
	// 按原序合并：总量闸只计成功附件（len(out)），失败不占名额，
	// 与串行版 `if len(out) >= maxAttachments` 语义一致。
	var out []extractedFile
	for i := range slots {
		if len(out) >= maxAttachments {
			log.Printf("attachment dropped (part %d name=%q): over the %d-attachment limit", slots[i].idx, slots[i].name, maxAttachments)
			continue
		}
		if slots[i].err != nil {
			log.Printf("attachment dropped (part %d type=%q name=%q): %v", slots[i].idx, slots[i].typ, slots[i].name, slots[i].err)
			continue
		}
		f := slots[i].file
		if textLike(f.Mime, f.Name) {
			// 抽成正文（PDF 抽字面量、文本原样，上限 8000 rune）。
			f.Text = emulation.ExtractDocument(f.Mime, "base64", base64.StdEncoding.EncodeToString(f.Data), "", f.Name)
		}
		out = append(out, f)
	}
	return out
}

// resolveAttachment 把一张附件载荷解成字节，并补上文件名与 MIME。
func resolveAttachment(ctx context.Context, name, payload string) (extractedFile, error) {
	switch {
	case strings.HasPrefix(payload, "data:"):
		mime, data, err := decodeDataURL(payload)
		if err != nil {
			return extractedFile{}, err
		}
		return extractedFile{Name: name, Mime: mime, Data: data}, nil
	case isRemoteURL(payload):
		data, mime, err := fetchAttachment(ctx, payload)
		if err != nil {
			return extractedFile{}, err
		}
		if name == "" {
			name = urlBaseName(payload)
		}
		return extractedFile{Name: name, Mime: mime, Data: data}, nil
	default:
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return extractedFile{}, fmt.Errorf("payload is neither a data: URL, an http(s) URL nor base64: %w", err)
		}
		if len(data) == 0 {
			return extractedFile{}, fmt.Errorf("empty payload")
		}
		return extractedFile{Name: name, Data: data}, nil
	}
}

// decodeDataURL 解析 data:[<mime>][;base64],<payload>。
func decodeDataURL(u string) (mime string, data []byte, err error) {
	comma := strings.Index(u, ",")
	if comma < 0 {
		return "", nil, fmt.Errorf("invalid data url")
	}
	meta, payload := u[len("data:"):comma], u[comma+1:]
	encoded := false
	for _, p := range strings.Split(meta, ";") {
		switch {
		case p == "base64":
			encoded = true
		case strings.Contains(p, "/"):
			mime = p
		}
	}
	if encoded {
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", nil, fmt.Errorf("decode data url: %w", err)
		}
		return mime, raw, nil
	}
	// 非 base64 的 data URL 是百分号编码的文本（RFC 2397）。
	if dec, err := url.PathUnescape(payload); err == nil {
		payload = dec
	}
	return mime, []byte(payload), nil
}

// fetchAttachment 代拉远端附件：http(s)、限时限量。
func fetchAttachment(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := attachmentClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch %s: status %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxAttachmentBytes {
		return nil, "", fmt.Errorf("fetch %s: larger than %d bytes", rawURL, maxAttachmentBytes)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("fetch %s: empty body", rawURL)
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	return data, mime, nil
}

func isRemoteURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// urlBaseName 取远端地址里的文件名（取不到就返回空，交给适配器生成）。
func urlBaseName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "/" || base == "." || base == "" || strings.Contains(base, "/") {
		return ""
	}
	return base
}
