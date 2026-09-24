package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"prism-2api/internal/adapter"
)

// 三个入口发来的附件块形状不同，但都必须落到 adapter.File（字节 + MIME + 名字）。
// 漏认形状 = 模型看不到图，而且不会有任何报错。
func TestExtractFilesAcrossProtocolShapes(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\npayload")
	b64 := base64.StdEncoding.EncodeToString(png)
	cases := []struct {
		name    string
		content string
		want    []extractedFile
	}{
		{
			name:    "openai image_url data url",
			content: `[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]`,
			want:    []extractedFile{{Mime: "image/png", Data: png}},
		},
		{
			name:    "openai file part",
			content: `[{"type":"file","file":{"filename":"paper.pdf","file_data":"data:application/pdf;base64,` + b64 + `"}}]`,
			want:    []extractedFile{{Name: "paper.pdf", Mime: "application/pdf", Data: png}},
		},
		{
			name:    "responses input_file",
			content: `[{"type":"input_file","filename":"notes.txt","file_data":"` + b64 + `"}]`,
			want:    []extractedFile{{Name: "notes.txt", Data: png}},
		},
		{
			name:    "anthropic 转出的 image + text",
			content: `[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + b64 + `"}}]`,
			want:    []extractedFile{{Mime: "image/jpeg", Data: png}},
		},
		{
			name:    "纯文本没有附件",
			content: `[{"type":"text","text":"你好"}]`,
			want:    nil,
		},
		{
			name:    "垃圾载荷只丢自己",
			content: `[{"type":"image_url","image_url":{"url":"data:image/png;base64,@@not-base64@@"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]`,
			want:    []extractedFile{{Mime: "image/png", Data: png}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractFiles(context.Background(), json.RawMessage(c.content))
			if len(got) != len(c.want) {
				t.Fatalf("extractFiles = %d 个附件 %+v, want %d %+v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i].Name != c.want[i].Name || got[i].Mime != c.want[i].Mime || string(got[i].Data) != string(c.want[i].Data) {
					t.Errorf("附件 %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// 远端地址由服务端代拉（很多客户端直接传 URL 而不是 data: URL）。
func TestExtractFilesFetchesRemoteURL(t *testing.T) {
	body := []byte("remote-image-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	got := extractFiles(context.Background(), json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"`+ts.URL+`/shots/cat.webp"}}]`))
	if len(got) != 1 {
		t.Fatalf("got %+v, want one fetched attachment", got)
	}
	if got[0].Mime != "image/webp" || string(got[0].Data) != string(body) || got[0].Name != "cat.webp" {
		t.Fatalf("远端附件 = %+v (name 应取 URL 末段、MIME 取响应头)", got[0])
	}
}

// 拉不到就别把请求带崩：附件丢掉、其余内容照常。
func TestExtractFilesDropsUnreachableURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	got := extractFiles(context.Background(), json.RawMessage(
		`[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"`+ts.URL+`/gone.png"}}]`))
	if len(got) != 0 {
		t.Fatalf("404 的远端附件应被丢弃，got %+v", got)
	}
}

// 端到端：/v1/chat/completions 里的图片必须进入 kernel 的 adapter.File。
// 这是"图片输入"从协议层到适配器的交接点，漏了它图片就静默消失。
func TestToAdapterRequestCarriesAttachments(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	req := &ChatCompletionRequest{Model: "gpt-6-astra", Messages: []ChatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"这个里面有啥东西"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]`)},
	}}
	out := toAdapterRequest(context.Background(), req, "注入")
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	m := out.Messages[0]
	if m.Content != "这个里面有啥东西" {
		t.Fatalf("文本块应摊平成 Content，got %q", m.Content)
	}
	if len(m.Files) != 1 || m.Files[0].Mime != "image/png" || string(m.Files[0].Data) != "png-bytes" {
		t.Fatalf("附件没带进 adapter.File: %+v", m.Files)
	}
	var _ adapter.File = m.Files[0]
}

// Gemini 的 inlineData 也要能进同一套附件链路（图片走 image_url、其余走 file）。
func TestGeminiInlineDataBecomesAttachment(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	body := `{"contents":[{"role":"user","parts":[{"text":"图里是啥"},
		{"inlineData":{"mimeType":"image/png","data":"` + b64 + `"}},
		{"inlineData":{"mimeType":"application/pdf","data":"` + b64 + `"}}]}]}`
	var req geminiRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, err := geminiToChatRequest(&req, "gemini-2.5-pro")
	if err != nil {
		t.Fatal(err)
	}
	files := extractFiles(context.Background(), chat.Messages[0].Content)
	if len(files) != 2 {
		t.Fatalf("inlineData 应产出 2 个附件，got %+v", files)
	}
	if files[0].Mime != "image/png" || files[1].Mime != "application/pdf" {
		t.Fatalf("MIME 应原样保留，got %+v", files)
	}
}

// Responses 的 input_file 块要摊平成 chat 的 file 块，才能被 extractFiles 认出来。
func TestFlattenResponsesContentKeepsFiles(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("pdf-bytes"))
	content := []any{
		map[string]any{"type": "input_text", "text": "总结这份文档"},
		map[string]any{"type": "input_file", "filename": "paper.pdf", "file_data": "data:application/pdf;base64," + b64},
		map[string]any{"type": "input_file", "file_id": "file-abc"},
	}
	flat, ok := flattenResponsesContent(content)
	if !ok {
		t.Fatal("数组输入应被摊平")
	}
	blob := mustJSON(flat)
	files := extractFiles(context.Background(), json.RawMessage(blob))
	if len(files) != 1 {
		t.Fatalf("只认 file_data（file_id 无文件存储可解），got %+v", files)
	}
	if files[0].Name != "paper.pdf" || files[0].Mime != "application/pdf" || string(files[0].Data) != "pdf-bytes" {
		t.Fatalf("file 块 = %+v", files[0])
	}
	if !strings.Contains(blob, `"type":"text"`) {
		t.Fatalf("文本块要保留，got %s", blob)
	}
}

// T1.5：远端附件并行代拉。三个远端延迟不同，总耗时约等于最慢一个而非三者之和。
// 任一失败只丢该附件（404 的那台丢掉，另外两台照常）；总量 maxAttachments=16 与
// 单文件 attachmentFetchTimeout=20s 语义不变（本单测不触达上限，只定并行语义）。
func TestExtractFilesFetchesRemoteURLsConcurrently(t *testing.T) {
	delays := map[string]time.Duration{
		"/slow-a.bin": 300 * time.Millisecond,
		"/slow-b.bin": 400 * time.Millisecond,
		"/slow-c.bin": 600 * time.Millisecond,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := delays[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(d)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("body:" + r.URL.Path))
	}))
	defer ts.Close()

	raw := json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"` + ts.URL + `/slow-a.bin"}},` +
			`{"type":"image_url","image_url":{"url":"` + ts.URL + `/slow-b.bin"}},` +
			`{"type":"image_url","image_url":{"url":"` + ts.URL + `/slow-c.bin"}}]`)
	start := time.Now()
	got := extractFiles(context.Background(), raw)
	elapsed := time.Since(start)
	if len(got) != 3 {
		t.Fatalf("三个远端都应拉回，got %+v", got)
	}
	// 原序合并：返回顺序应与 parts 顺序一致。
	for i, want := range []string{"body:/slow-a.bin", "body:/slow-b.bin", "body:/slow-c.bin"} {
		if string(got[i].Data) != want {
			t.Fatalf("附件 %d = %+v, want data %q（应保持原序）", i, got[i], want)
		}
	}
	// 并行上限：600ms（最慢）< 总耗时 < 1300ms（三者之和）。留足 CI 抖动余量。
	if elapsed >= 1300*time.Millisecond {
		t.Fatalf("远端拉取疑似串行：elapsed=%v（应远小于三者之和 1300ms）", elapsed)
	}
	if elapsed < 600*time.Millisecond {
		t.Fatalf("elapsed=%v 小于最慢远端 600ms，计时或 sleep 语义异常", elapsed)
	}
}

// T1.5：并发下任一失败只丢该附件，其余照常。
func TestExtractFilesConcurrentFetchDropsOnlyFailed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok-a.bin":
			time.Sleep(200 * time.Millisecond)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("ok-a"))
		case "/gone.bin":
			w.WriteHeader(http.StatusNotFound)
		case "/ok-b.bin":
			time.Sleep(200 * time.Millisecond)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("ok-b"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	got := extractFiles(context.Background(), json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"`+ts.URL+`/ok-a.bin"}},`+
			`{"type":"image_url","image_url":{"url":"`+ts.URL+`/gone.bin"}},`+
			`{"type":"image_url","image_url":{"url":"`+ts.URL+`/ok-b.bin"}}]`))
	if len(got) != 2 {
		t.Fatalf("404 只丢自己，got %+v", got)
	}
	if string(got[0].Data) != "ok-a" || string(got[1].Data) != "ok-b" {
		t.Fatalf("幸存附件应保持原序，got %+v", got)
	}
}
