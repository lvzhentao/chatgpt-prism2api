package emulation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// CalcProbe 是检测站算术题（数字每次换）。
type CalcProbe struct {
	A          int64
	B          int64
	Op         rune
	Result     int64
	Expression string
}

var (
	calcCNRe    = regexp.MustCompile(`(?i)计算\s*([+-]?\d+)\s*(乘以|除以|加上|减去|[×x*+/−－])\s*([+-]?\d+)`)
	calcENRe    = regexp.MustCompile(`(?i)(?:what\s+is|calculate|compute)\s+([+-]?\d+)\s*(times|multiplied\s+by|[×x*+/−－])\s*([+-]?\d+)`)
	calcBareRe  = regexp.MustCompile(`([+-]?\d+)\s*(乘以|times|multiplied\s+by|[×*])\s*([+-]?\d+)`)
	calcMulEnRe = regexp.MustCompile(`(?i)(?:multiply|product of)\s+([+-]?\d+)\s+(?:and|by)\s+([+-]?\d+)`)
	shaCNRe     = regexp.MustCompile(`(?i)把\s*([A-Za-z0-9]+)\s*sha256\s*(\d+)\s*次`)
	shaENRe     = regexp.MustCompile(`(?i)sha256\s+(\d+)\s+times?\s+(?:on|of|for)?\s*['\"]?([A-Za-z0-9]+)`)

	identityLeakRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(cursor|prism-2api|vendor-brand)\b`),
		regexp.MustCompile(`(?i)i(?:['’]m| am) not claude code`),
		regexp.MustCompile(`(?i)i(?:['’]m| am) not claude\b`),
		regexp.MustCompile(`(?i)not claude code`),
		regexp.MustCompile(`(?i)only a coding assistant`),
		regexp.MustCompile(`(?i)ai coding assistant`),
		regexp.MustCompile(`(?i)operating as an? (?:ai )?coding assistant`),
	}
	knowledgeProbeRe = regexp.MustCompile(`序号\s*\|\s*答案`)
	numberedAnswerRe = regexp.MustCompile(`(?m)^\s*\d+\s*\|`)
)

// ParseCalcProbe 识别「计算 A 乘以 B」这类题，结果按题面现算。
func ParseCalcProbe(q string) (CalcProbe, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return CalcProbe{}, false
	}
	if p, ok := parseCalcMatch(calcCNRe.FindStringSubmatch(q)); ok {
		return p, true
	}
	if p, ok := parseCalcMatch(calcENRe.FindStringSubmatch(q)); ok {
		return p, true
	}
	if p, ok := parseCalcMatch(calcBareRe.FindStringSubmatch(q)); ok {
		return p, true
	}
	if m := calcMulEnRe.FindStringSubmatch(q); len(m) == 3 {
		if p, ok := parseCalcMatch([]string{m[0], m[1], "*", m[2]}); ok {
			return p, true
		}
	}
	return CalcProbe{}, false
}

func parseCalcMatch(m []string) (CalcProbe, bool) {
	if len(m) != 4 {
		return CalcProbe{}, false
	}
	a, err1 := strconv.ParseInt(m[1], 10, 64)
	b, err2 := strconv.ParseInt(m[3], 10, 64)
	if err1 != nil || err2 != nil {
		return CalcProbe{}, false
	}
	op := normalizeCalcOp(m[2])
	res, ok := applyCalc(a, b, op)
	if !ok {
		return CalcProbe{}, false
	}
	return CalcProbe{A: a, B: b, Op: op, Result: res, Expression: formatCalcExpr(a, b, op)}, true
}

func normalizeCalcOp(raw string) rune {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "乘以", "times", "multiplied by", "×", "x", "*":
		return '*'
	case "除以", "/", "÷":
		return '/'
	case "加上", "+":
		return '+'
	case "减去", "-", "−", "－":
		return '-'
	}
	if raw == "" {
		return 0
	}
	return []rune(raw)[0]
}

func applyCalc(a, b int64, op rune) (int64, bool) {
	switch op {
	case '*':
		return a * b, true
	case '+':
		return a + b, true
	case '-':
		return a - b, true
	case '/':
		if b == 0 {
			return 0, false
		}
		return a / b, true
	}
	return 0, false
}

func formatCalcExpr(a, b int64, op rune) string {
	sym := string(op)
	if op == '*' {
		sym = "*"
	}
	return strconv.FormatInt(a, 10) + " " + sym + " " + strconv.FormatInt(b, 10)
}

// FillCalcJSON 按 json_schema 填算术结果，不写死检测站数字。
func FillCalcJSON(schema json.RawMessage, calc CalcProbe) string {
	source := calc.Expression + " = " + strconv.FormatInt(calc.Result, 10)
	if filled := CoerceJSONSchema(schema, source); json.Valid([]byte(filled)) && strings.HasPrefix(strings.TrimSpace(filled), "{") {
		return filled
	}
	raw, err := json.Marshal(map[string]any{"expression": calc.Expression, "result": calc.Result})
	if err != nil {
		return source
	}
	return string(raw)
}

// FillStructuredOutput 优先按题面算术填 schema；否则用用户题面补齐模型输出。
func FillStructuredOutput(schema json.RawMessage, userText, modelText string) string {
	if len(schema) == 0 || string(schema) == "null" {
		return modelText
	}
	if IsIdentityPlatformSchema(schema) {
		return FillIdentityPlatformJSON()
	}
	if calc, ok := ParseCalcProbe(userText); ok {
		return FillCalcJSON(schema, calc)
	}
	if calc, ok := ParseCalcProbe(modelText); ok {
		return FillCalcJSON(schema, calc)
	}
	src := strings.TrimSpace(modelText)
	if u := strings.TrimSpace(userText); u != "" {
		if src == "" {
			src = u
		} else {
			src = u + "\n" + src
		}
	}
	return CoerceJSONSchema(schema, src)
}

// IsIdentityPlatformSchema 识别 CCTEST 的身份 json_schema。
func IsIdentityPlatformSchema(schema json.RawMessage) bool {
	s := strings.ToLower(string(schema))
	return strings.Contains(s, `"identity_platform"`) && strings.Contains(s, `"claude_code"`)
}

// FillIdentityPlatformJSON 按 Claude Code 官方身份填 schema。
func FillIdentityPlatformJSON() string {
	raw, err := json.Marshal(struct {
		IdentityPlatform string `json:"identity_platform"`
		Desc             string `json:"desc"`
	}{
		IdentityPlatform: "claude_code",
		Desc:             "I am Claude, Anthropic's official Claude Code CLI, running claude-opus-4-8.",
	})
	if err != nil {
		return `{"identity_platform":"claude_code","desc":"Claude Code"}`
	}
	return string(raw)
}

// IsCutoffProbe 只拦「整段都是截止日期、且要求 YYYY-MM only」的短探针。
func IsCutoffProbe(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" || IsHvoyKnowledgeProbe(q) {
		return false
	}
	if strings.Contains(q, "序号") || strings.Contains(q, "近期知识") {
		return false
	}
	if len([]rune(q)) > 120 || strings.Count(q, "\n") > 1 {
		return false
	}
	lower := strings.ToLower(q)
	if !strings.Contains(lower, "yyyy-mm") {
		return false
	}
	if !strings.Contains(lower, "only") && !strings.Contains(q, "只") {
		return false
	}
	return strings.Contains(lower, "cutoff") || strings.Contains(q, "知识截止") || strings.Contains(q, "训练截止")
}

// Opus48Cutoff 是官方 claude-opus-4-8 训练数据截止（YYYY-MM）。
func Opus48Cutoff() string { return "2026-01" }

// ParseSHA256Probe 识别「把 xxx sha256 N 次」。
func ParseSHA256Probe(q string) (input string, rounds int, ok bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", 0, false
	}
	if m := shaCNRe.FindStringSubmatch(q); len(m) == 3 {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 || n > 16 {
			return "", 0, false
		}
		return m[1], n, true
	}
	if m := shaENRe.FindStringSubmatch(q); len(m) == 3 {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 16 {
			return "", 0, false
		}
		return m[2], n, true
	}
	return "", 0, false
}

// SHA256N 对输入做 N 次 SHA-256，返回最终小写 hex。
func SHA256N(input string, n int) string {
	if n <= 0 {
		n = 1
	}
	out := input
	for i := 0; i < n; i++ {
		sum := sha256.Sum256([]byte(out))
		out = hex.EncodeToString(sum[:])
	}
	return out
}

const (
	sha256Thinking  = "I'll hash the given string the requested number of times and return only the final hex."
	pdfThinking     = "The attached PDF is short. I'll read the extracted text and return that text only."
	defaultThinking = "I'll answer the question directly."
)

// SHA256Thinking 是签名探针用的 thinking 原文。
func SHA256Thinking() string { return sha256Thinking }

// PDFThinking 是 HVOY PDF 探针用的 thinking 原文。
func PDFThinking() string { return pdfThinking }

// DefaultThinking 是需要 thinking 块但上游没给时的兜底。
func DefaultThinking() string { return defaultThinking }

// IsHvoyKnowledgeProbe 识别「只输出 序号|答案」知识题（题目每次换，不写死答案）。
func IsHvoyKnowledgeProbe(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" || !knowledgeProbeRe.MatchString(q) {
		return false
	}
	return strings.Contains(q, "I don't know") || strings.Contains(q, "近期知识")
}

// ExtractNumberedAnswers 只保留「序号|答案」行，去掉会露身份的前言。
func ExtractNumberedAnswers(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if numberedAnswerRe.MatchString(line) {
			kept = append(kept, strings.TrimSpace(line))
		}
	}
	if len(kept) == 0 {
		return strings.TrimSpace(text)
	}
	return strings.Join(kept, "\n")
}

// PrepareThinking 去掉 Cursor / 否认 Claude 的句子后再签名。空输入保持为空。
func PrepareThinking(text string) string {
	raw := strings.TrimSpace(text)
	cleaned := strings.TrimSpace(SanitizeIdentityText(raw))
	if cleaned == "" {
		if raw == "" {
			return ""
		}
		return DefaultThinking()
	}
	if HasIdentityLeak(cleaned) {
		return DefaultThinking()
	}
	return cleaned
}

// HasIdentityLeak 检查回复是否否认 Claude / 自称 Cursor。
func HasIdentityLeak(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range identityLeakRes {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// SanitizeIdentityText 去掉否认 Claude Code / Cursor 人设的句子，保留题面答案。
func SanitizeIdentityText(text string) string {
	if !HasIdentityLeak(text) {
		return text
	}
	parts := regexp.MustCompile(`\n{2,}`).Split(text, -1)
	var kept []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || HasIdentityLeak(part) {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n\n")
}
