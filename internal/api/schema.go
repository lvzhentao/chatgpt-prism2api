package api

import (
	"encoding/json"
)

// normalizeJSONSchema 规范化 JSON Schema，修复 MCP 工具定义中常见的类型问题
// （移植自 kiro.rs src/anthropic/converter/schema.rs，经其灰度实测验证）。
//
// Claude Code / MCP 工具定义偶尔会出现 `required: null`、`properties: null` 等，
// 导致上游返回 400 "Improperly formed request" 或工具声明被静默忽略。
//
// 策略（与 kiro.rs 一致）：
//   - $schema / additionalProperties：缺失时不补（省字节、避免 prefix-cache 偏移），
//     仅在存在但形态非法时纠正。
//   - type / properties / required：真正的治理目标，缺失补默认。
func normalizeJSONSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw // 非 JSON 原样透传，避免破坏调用方
	}

	// $schema：存在但非合法非空字符串才删除（缺失不补）。
	if v, ok := m["$schema"]; ok {
		s, isStr := v.(string)
		if !isStr || s == "" {
			delete(m, "$schema")
		}
	}

	// type（必须是字符串）：缺失/非法 → "object"。
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	} else if s, isStr := m["type"].(string); !isStr || s == "" {
		m["type"] = "object"
	}

	// properties（必须是 object）：缺失/null/非法 → 空对象。
	switch v := m["properties"].(type) {
	case nil:
		m["properties"] = map[string]any{}
	case map[string]any:
		m["properties"] = v
	default:
		m["properties"] = map[string]any{}
	}

	// required（必须是 string 数组）：缺失/null/含非法元素 → 空数组。
	m["required"] = normalizeRequired(m["required"])

	// additionalProperties：存在但非 bool/object 才纠正为 true（缺失不补）。
	if v, ok := m["additionalProperties"]; ok {
		switch v.(type) {
		case bool, map[string]any:
			// OK
		default:
			m["additionalProperties"] = true
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// normalizeRequired 规整 required 字段为合法 string 数组。
func normalizeRequired(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, isStr := item.(string); isStr && s != "" {
			out = append(out, s)
		}
	}
	return out
}
