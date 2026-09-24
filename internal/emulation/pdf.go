package emulation

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const pdfTextLimit = 8000

var hvoyReportTotalRe = regexp.MustCompile(`(?i)Hvoy AI report total\s+(\d+)`)

// ExtractDocument 把 Anthropic document source 抽成可喂给模型的文本。
func ExtractDocument(mediaType, sourceType, data, url, title string) string {
	sourceType = strings.ToLower(strings.TrimSpace(sourceType))
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch sourceType {
	case "text":
		text := strings.TrimSpace(data)
		if text == "" {
			return ""
		}
		return wrapDocumentText(title, "text", len(text), text)
	case "url":
		if strings.TrimSpace(url) == "" {
			return ""
		}
		return fmt.Sprintf("[Attached document url: %s title=%s]", strings.TrimSpace(url), title)
	case "file":
		return fmt.Sprintf("[Attached document file_id=%s title=%s]", strings.TrimSpace(data), title)
	case "base64", "":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(data))
		}
		if err != nil || len(raw) == 0 {
			return ""
		}
		if strings.Contains(mediaType, "pdf") || bytes.HasPrefix(raw, []byte("%PDF")) {
			return extractPDFDocument(raw, title)
		}
		if strings.HasPrefix(mediaType, "text/") || looksLikeUTF8(raw) {
			return wrapDocumentText(title, mediaType, len(raw), string(raw))
		}
		return fmt.Sprintf("[Attached binary document title=%s media=%s bytes=%d]", title, mediaType, len(raw))
	default:
		return ""
	}
}

func extractPDFDocument(raw []byte, title string) string {
	text := strings.TrimSpace(ExtractPDFText(raw))
	if utf8.RuneCountInString(text) > pdfTextLimit {
		text = truncateRunes(text, pdfTextLimit) + "\n[PDF text truncated]"
	}
	sum := sha256.Sum256(raw)
	if title == "" {
		title = "document.pdf"
	}
	return fmt.Sprintf("[Attached PDF document: %s, bytes=%d, sha256=%x]\n[Extracted PDF text]\n%s\n[/Extracted PDF text]",
		title, len(raw), sum[:8], text)
}

func wrapDocumentText(title, media string, bytes int, text string) string {
	if title == "" {
		title = "document"
	}
	if utf8.RuneCountInString(text) > pdfTextLimit {
		text = truncateRunes(text, pdfTextLimit) + "\n[text truncated]"
	}
	return fmt.Sprintf("[Attached document: %s media=%s bytes=%d]\n%s", title, media, bytes, text)
}

// ExtractPDFText 轻量抽取 PDF 字面量（含 Flate 流）。
func ExtractPDFText(data []byte) string {
	chunks := [][]byte{data}
	chunks = append(chunks, inflatePDFStreams(data)...)
	var lines []string
	seen := map[string]bool{}
	for _, chunk := range chunks {
		for _, text := range extractPDFStrings(chunk) {
			text = strings.Join(strings.Fields(text), " ")
			if text == "" || seen[text] || !looksLikeReadableText(text) {
				continue
			}
			seen[text] = true
			lines = append(lines, text)
			if len(lines) >= 240 {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// HvoyReportTotal 抽出 HVOY PDF 探针里的动态 token，不写死数字。
func HvoyReportTotal(text string) string {
	m := hvoyReportTotalRe.FindStringSubmatch(text)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func inflatePDFStreams(data []byte) [][]byte {
	var out [][]byte
	searchFrom := 0
	for {
		rel := bytes.Index(data[searchFrom:], []byte("stream"))
		if rel < 0 {
			break
		}
		streamPos := searchFrom + rel + len("stream")
		if streamPos < len(data) && data[streamPos] == '\r' {
			streamPos++
		}
		if streamPos < len(data) && data[streamPos] == '\n' {
			streamPos++
		}
		endRel := bytes.Index(data[streamPos:], []byte("endstream"))
		if endRel < 0 {
			break
		}
		endPos := streamPos + endRel
		raw := bytes.TrimSpace(data[streamPos:endPos])
		if len(raw) > 0 {
			if r, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
				if decoded, err := io.ReadAll(io.LimitReader(r, 2<<20)); err == nil && len(decoded) > 0 {
					out = append(out, decoded)
				}
				_ = r.Close()
			}
		}
		searchFrom = endPos + len("endstream")
	}
	return out
}

func extractPDFStrings(data []byte) []string {
	var out []string
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '(':
			text, next := readPDFLiteral(data, i+1)
			if text != "" {
				out = append(out, text)
			}
			i = next
		case '<':
			if i+1 < len(data) && data[i+1] == '<' {
				continue
			}
			if text, next := readPDFHex(data, i+1); next > i {
				if text != "" {
					out = append(out, text)
				}
				i = next
			}
		}
	}
	return out
}

func readPDFLiteral(data []byte, pos int) (string, int) {
	var out []byte
	depth := 1
	for i := pos; i < len(data); i++ {
		ch := data[i]
		if ch == '\\' && i+1 < len(data) {
			i++
			next := data[i]
			switch next {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b', 'f':
			case '(', ')', '\\':
				out = append(out, next)
			case '\n', '\r':
			default:
				if next >= '0' && next <= '7' {
					val := int(next - '0')
					n := 1
					for n < 3 && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7' {
						i++
						val = val*8 + int(data[i]-'0')
						n++
					}
					out = append(out, byte(val))
				} else {
					out = append(out, next)
				}
			}
			continue
		}
		if ch == '(' {
			depth++
			out = append(out, ch)
			continue
		}
		if ch == ')' {
			depth--
			if depth == 0 {
				return string(out), i
			}
			out = append(out, ch)
			continue
		}
		out = append(out, ch)
	}
	return string(out), len(data) - 1
}

func readPDFHex(data []byte, pos int) (string, int) {
	end := bytes.IndexByte(data[pos:], '>')
	if end < 0 {
		return "", pos
	}
	hexStr := strings.ReplaceAll(string(data[pos:pos+end]), " ", "")
	if len(hexStr)%2 == 1 {
		hexStr += "0"
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return "", pos + end
	}
	if len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff && len(raw)%2 == 0 {
		var b strings.Builder
		for i := 2; i+1 < len(raw); i += 2 {
			r := rune(raw[i])<<8 | rune(raw[i+1])
			if unicode.IsPrint(r) || unicode.IsSpace(r) {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			return b.String(), pos + end
		}
	}
	if !looksLikeUTF8(raw) {
		return "", pos + end
	}
	return string(raw), pos + end
}

func looksLikeReadableText(s string) bool {
	if s == "" {
		return false
	}
	allDigit := true
	for _, r := range s {
		if !unicode.IsDigit(r) {
			allDigit = false
			break
		}
	}
	if allDigit {
		return true
	}
	if utf8.RuneCountInString(s) < 3 {
		return false
	}
	letters := 0
	total := 0
	for _, r := range s {
		if r == 0 || (!unicode.IsPrint(r) && !unicode.IsSpace(r)) {
			return false
		}
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	if total == 0 || letters*2 < total {
		return false
	}
	switch strings.TrimSpace(s) {
	case "obj", "endobj", "stream", "endstream", "xref", "trailer":
		return false
	}
	return true
}

func looksLikeUTF8(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return utf8.Valid(b)
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for idx := range s {
		if i == n {
			return s[:idx]
		}
		i++
	}
	return s
}
