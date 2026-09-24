package emulation

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// StructuredOutputInstruction 要求模型按 JSON Schema 只回一个对象。
func StructuredOutputInstruction(schema json.RawMessage) string {
	if len(schema) == 0 || string(schema) == "null" {
		return ""
	}
	return "[Structured output required]\nYou MUST reply with a single JSON object that validates against this JSON Schema. No markdown fences, no commentary, no extra keys.\n" + string(schema)
}

// CoerceJSONSchema 先抽出 JSON，再按 schema 补齐缺失字段（例如 "48 × 78 = 3744" → {expression, result}）。
func CoerceJSONSchema(schema json.RawMessage, text string) string {
	coerced := CoerceJSONText(text)
	if len(schema) == 0 || string(schema) == "null" {
		return coerced
	}
	filled, ok := fillJSONSchema(schema, coerced, text)
	if ok {
		return filled
	}
	return coerced
}

// CoerceJSONText 从模型输出里抽出 JSON 对象；失败则原样返回。
func CoerceJSONText(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return text
	}
	if json.Valid([]byte(s)) && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
		return s
	}
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			lang := strings.TrimSpace(rest[:nl])
			if strings.EqualFold(lang, "json") || lang == "" {
				rest = rest[nl+1:]
			}
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			inner := strings.TrimSpace(rest[:end])
			if json.Valid([]byte(inner)) {
				return inner
			}
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		inner := s[start : end+1]
		if json.Valid([]byte(inner)) {
			return inner
		}
	}
	return text
}

type jsonSchemaNode struct {
	Type                 any             `json:"type"`
	Properties           json.RawMessage `json:"properties"`
	Required             []string        `json:"required"`
	AdditionalProperties any             `json:"additionalProperties"`
}

func fillJSONSchema(schema json.RawMessage, coerced, original string) (string, bool) {
	var node jsonSchemaNode
	if err := json.Unmarshal(schema, &node); err != nil {
		return "", false
	}
	if !schemaIsObject(node.Type) {
		return "", false
	}
	var props map[string]json.RawMessage
	if len(node.Properties) > 0 && string(node.Properties) != "null" {
		_ = json.Unmarshal(node.Properties, &props)
	}
	out := map[string]any{}
	if json.Valid([]byte(strings.TrimSpace(coerced))) && strings.HasPrefix(strings.TrimSpace(coerced), "{") {
		if err := json.Unmarshal([]byte(coerced), &out); err != nil {
			out = map[string]any{}
		}
	}
	source := original
	if strings.TrimSpace(coerced) != "" && coerced != original {
		source = original
	}
	nums := parseNumbers(source)
	expr := expressionFromText(source)
	keys := node.Required
	if len(keys) == 0 {
		for k := range props {
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		prop := props[key]
		if val, exists := out[key]; exists && val != nil {
			out[key] = coerceSchemaValue(val, schemaType(prop), key, source, expr, nums)
			continue
		}
		out[key] = inferSchemaValue(key, prop, source, expr, nums)
	}
	if additionalPropertiesForbidden(node.AdditionalProperties) {
		allowed := map[string]bool{}
		for k := range props {
			allowed[k] = true
		}
		for k := range out {
			if !allowed[k] {
				delete(out, k)
			}
		}
	}
	raw, err := marshalSchemaObject(keys, props, out)
	if err != nil || !json.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

func additionalPropertiesForbidden(v any) bool {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return strings.EqualFold(t, "false")
	}
	return false
}

func coerceSchemaValue(val any, typ, key, source, expr string, nums []float64) any {
	switch typ {
	case "integer":
		if n, ok := anyToInt(val); ok {
			return n
		}
		return inferSchemaValue(key, json.RawMessage(`{"type":"integer"}`), source, expr, nums)
	case "number":
		if n, ok := anyToFloat(val); ok {
			return n
		}
		return inferSchemaValue(key, json.RawMessage(`{"type":"number"}`), source, expr, nums)
	case "boolean":
		switch v := val.(type) {
		case bool:
			return v
		case string:
			return strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		}
		return inferSchemaValue(key, json.RawMessage(`{"type":"boolean"}`), source, expr, nums)
	default:
		if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if isExpressionKey(strings.ToLower(key)) && expr != "" {
			return expr
		}
		return val
	}
}

func anyToInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i, err == nil
	}
	return 0, false
}

func anyToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func marshalSchemaObject(required []string, props map[string]json.RawMessage, out map[string]any) ([]byte, error) {
	order := make([]string, 0, len(out))
	seen := map[string]bool{}
	for _, k := range required {
		if _, ok := out[k]; ok && !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	for k := range props {
		if _, ok := out[k]; ok && !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	for k := range out {
		if !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	buf := strings.Builder{}
	buf.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(out[k])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

func schemaIsObject(t any) bool {
	switch v := t.(type) {
	case string:
		return strings.EqualFold(v, "object") || v == ""
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.EqualFold(s, "object") {
				return true
			}
		}
	}
	return t == nil
}

func inferSchemaValue(key string, prop json.RawMessage, source, expr string, nums []float64) any {
	typ := schemaType(prop)
	lower := strings.ToLower(key)
	switch typ {
	case "integer":
		if n, ok := pickNumber(lower, nums); ok {
			return int64(n)
		}
		return 0
	case "number":
		if n, ok := pickNumber(lower, nums); ok {
			return n
		}
		return 0
	case "boolean":
		return strings.Contains(strings.ToLower(source), "true") || strings.Contains(strings.ToLower(source), "yes")
	default:
		if isExpressionKey(lower) && expr != "" {
			return expr
		}
		if strings.TrimSpace(source) != "" {
			return strings.TrimSpace(source)
		}
		return ""
	}
}

func schemaType(prop json.RawMessage) string {
	if len(prop) == 0 {
		return "string"
	}
	var node struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(prop, &node); err != nil {
		return "string"
	}
	switch v := node.Type.(type) {
	case string:
		return strings.ToLower(v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "null" {
				return strings.ToLower(s)
			}
		}
	}
	return "string"
}

func isExpressionKey(key string) bool {
	switch key {
	case "expression", "formula", "equation", "expr", "problem":
		return true
	}
	return false
}

func pickNumber(key string, nums []float64) (float64, bool) {
	if len(nums) == 0 {
		return 0, false
	}
	if isExpressionKey(key) {
		return 0, false
	}
	return nums[len(nums)-1], true
}

func expressionFromText(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "="); i > 0 {
		left := strings.TrimSpace(s[:i])
		if left != "" {
			return left
		}
	}
	return s
}

var numberRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

func parseNumbers(text string) []float64 {
	var out []float64
	for _, m := range numberRe.FindAllString(text, -1) {
		n, err := strconv.ParseFloat(m, 64)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}
