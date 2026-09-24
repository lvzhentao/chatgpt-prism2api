package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// contentText 尽量把 content 字段提取为文本（兼容 string / 数组）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []ContentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text":
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

// SSEWriter 是流式响应写出器。
type SSEWriter struct {
	mu      sync.Mutex // 心跳 goroutine 与主循环并发写（Event/Done），必须互斥
	w       io.Writer
	flusher interface{ Flush() }
}

// NewSSEWriter 创建 SSE 写出器。
func NewSSEWriter(w io.Writer) *SSEWriter {
	sw := &SSEWriter{w: w}
	if f, ok := w.(interface{ Flush() }); ok {
		sw.flusher = f
	}
	return sw
}

// Event 写出一条 SSE 事件。
func (s *SSEWriter) Event(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

// Done 写出流结束标记。
func (s *SSEWriter) Done() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return err
}

var (
	toolResultMaxChars  = 8000
	toolResultHeadChars = 4000
	toolResultTailChars = 1000
	historyMaxChars     = 120000
)

// ApplyHistoryCompress sets history compression: off | light | code | standard | aggressive.
func ApplyHistoryCompress(tier string) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "off":
		toolResultMaxChars, toolResultHeadChars, toolResultTailChars, historyMaxChars = 1<<30, 1<<30, 0, 1<<30
	case "light":
		toolResultMaxChars, toolResultHeadChars, toolResultTailChars, historyMaxChars = 16000, 8000, 2000, 240000
	case "code":
		// 给编程 agent 用：standard 的 8000 字符/条会把一次普通文件读取从中间切掉，
		// 模型拿着残缺内容改代码且不报错。48k 够放一份千行文件，总量仍留边界。
		toolResultMaxChars, toolResultHeadChars, toolResultTailChars, historyMaxChars = 48000, 24000, 6000, 320000
	case "aggressive":
		toolResultMaxChars, toolResultHeadChars, toolResultTailChars, historyMaxChars = 4000, 2000, 400, 40000
	default:
		toolResultMaxChars, toolResultHeadChars, toolResultTailChars, historyMaxChars = 8000, 4000, 1000, 120000
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
