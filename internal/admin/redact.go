package admin

import (
	"encoding/json"
	"regexp"
	"strings"
)

// BodyLimitBytes 请求体落日志上限（对齐 navos request-log-service 32KB）。
const BodyLimitBytes = 32 * 1024

const (
	maxRedactDepth = 8
	maxArrayItems  = 200
	maxMessages    = 40
	maxStringChars = 8000
)

var redactHeaderKeys = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-master-key":        true,
	"cookie":              true,
	"set-cookie":          true,
}

var fullyRedactedBodyKeys = map[string]bool{
	"password":      true,
	"pwd":           true,
	"refresh_token": true,
	"refreshtoken":  true,
}

var redactedBodyKeys = map[string]bool{
	"password":      true,
	"pwd":           true,
	"token":         true,
	"refresh_token": true,
	"refreshtoken":  true,
	"api_key":       true,
	"apikey":        true,
	"api-key":       true,
	"secret":        true,
	"authorization": true,
	"access_token":  true,
	"client_secret": true,
}

var (
	bearerRE = regexp.MustCompile(`(?i)(bearer\s+)\S+`)
	secretRE = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret)\s*[:=]\s*)\S+`)
	cskRE    = regexp.MustCompile(`csk_[A-Za-z0-9]+`)
	skRE     = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
)

// SanitizeHeaders 脱敏敏感头（Authorization / Cookie / x-api-key 等）。
func SanitizeHeaders(h map[string][]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		key := strings.ToLower(k)
		raw := strings.Join(vs, ",")
		out[key] = redactHeaderValue(key, raw)
	}
	return out
}

func redactHeaderValue(header, raw string) string {
	if !redactHeaderKeys[header] {
		return raw
	}
	if header == "authorization" || header == "proxy-authorization" {
		return maskAuthorization(raw)
	}
	return maskShortSecret(raw)
}

func maskAuthorization(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}
	scheme, rest, ok := strings.Cut(trimmed, " ")
	if !ok || strings.TrimSpace(rest) == "" {
		return prefixMask(scheme)
	}
	value := strings.TrimSpace(rest)
	keep := value
	if len(keep) > 6 {
		keep = keep[:6]
	}
	return scheme + " " + keep + "***(" + itoa(len(value)) + ")"
}

func maskShortSecret(raw string) string {
	if len(raw) <= 4 {
		return "***"
	}
	return raw[:4] + "***(" + itoa(len(raw)) + ")"
}

func prefixMask(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return s[:4] + "***"
}

// ParseJSONPayload 尝试把原始字节当 JSON；失败则当字符串。
func ParseJSONPayload(raw []byte) any {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return nil
	}
	if t[0] == '{' || t[0] == '[' {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			return v
		}
	}
	return t
}

// RedactBody 递归打码 token / password / api_key 等字段（对齐 navos redactBody）。
func RedactBody(value any) any {
	return redactBody(value, 0)
}

func redactBody(value any, depth int) any {
	if depth > maxRedactDepth {
		return "[MAX_DEPTH]"
	}
	switch v := value.(type) {
	case nil:
		return nil
	case []any:
		n := len(v)
		if n > maxArrayItems {
			n = maxArrayItems
		}
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = redactBody(v[i], depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			lower := strings.ToLower(k)
			if fullyRedactedBodyKeys[lower] {
				out[k] = "[REDACTED]"
				continue
			}
			if redactedBodyKeys[lower] {
				if s, ok := item.(string); ok {
					out[k] = maskShortSecret(s)
				} else {
					out[k] = "[REDACTED]"
				}
				continue
			}
			if lower == "messages" {
				if arr, ok := item.([]any); ok {
					n := len(arr)
					if n > maxMessages {
						n = maxMessages
					}
					clipped := make([]any, n)
					for i := 0; i < n; i++ {
						clipped[i] = redactBody(arr[i], depth+1)
					}
					out[k] = clipped
					continue
				}
			}
			out[k] = redactBody(item, depth+1)
		}
		return out
	case string:
		if len(v) > maxStringChars {
			return truncateHeadTail(v, maxStringChars)
		}
		return v
	default:
		return value
	}
}

// RedactRequestBody 解析并脱敏请求体，再截断到 BodyLimitBytes。
func RedactRequestBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return SafeStringify(RedactBody(ParseJSONPayload(raw)), BodyLimitBytes)
}

// RedactText 打码自由文本里的 Bearer / api_key / csk_ / sk-（错误信息用）。
func RedactText(s string) string {
	if s == "" {
		return s
	}
	s = bearerRE.ReplaceAllString(s, "${1}***")
	s = secretRE.ReplaceAllString(s, "${1}***")
	s = cskRE.ReplaceAllString(s, "csk_***")
	s = skRE.ReplaceAllString(s, "sk-***")
	return s
}

// SafeStringify JSON 序列化并按字节上限截断。
func SafeStringify(value any, maxBytes int) string {
	if value == nil {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = BodyLimitBytes
	}
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "[unserializable]"
		}
		raw = string(b)
	}
	if len(raw) <= maxBytes {
		return raw
	}
	return truncateHeadTail(raw, maxBytes)
}

func truncateHeadTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	keep := max / 2
	if keep < 32 {
		keep = 32
	}
	if keep*2 >= len(s) {
		return s
	}
	return s[:keep] + "...[truncated " + itoa(len(s)-keep*2) + " chars]..." + s[len(s)-keep:]
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
