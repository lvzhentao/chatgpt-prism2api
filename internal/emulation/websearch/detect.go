package websearch

import (
	"encoding/json"
	"strings"
)

// IsOnlyWebSearch 判断请求是否只有一个 web_search 类工具。
func IsOnlyWebSearch(body []byte) bool {
	tools := parseTools(body)
	return len(tools) == 1 && isWebSearchTool(tools[0].Type, tools[0].Name)
}

// HasWebSearch 判断请求是否带了 web_search 工具。
func HasWebSearch(body []byte) bool {
	for _, t := range parseTools(body) {
		if isWebSearchTool(t.Type, t.Name) {
			return true
		}
	}
	return false
}

// IsOnlyCodeExecution 判断请求是否只有一个 code_execution 类工具。
func IsOnlyCodeExecution(body []byte) bool {
	tools := parseTools(body)
	return len(tools) == 1 && isCodeExecutionTool(tools[0].Type, tools[0].Name)
}

// ExtractQuery 取最后一条 user 文本作为搜索词。
func ExtractQuery(body []byte) string {
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role != "user" {
			continue
		}
		if q := lastTextFromContent(payload.Messages[i].Content); q != "" {
			return q
		}
	}
	return ""
}

type toolRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func parseTools(body []byte) []toolRef {
	var payload struct {
		Tools []toolRef `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload.Tools
}

func isWebSearchTool(typ, name string) bool {
	typ = strings.ToLower(strings.TrimSpace(typ))
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(typ, "web_search") || typ == "google_search" {
		return true
	}
	switch name {
	case "web_search", "google_search", "web_search_20250305", "websearch":
		return true
	}
	return false
}

func isCodeExecutionTool(typ, name string) bool {
	typ = strings.ToLower(strings.TrimSpace(typ))
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(typ, "code_execution") {
		return true
	}
	return name == "code_execution" || name == "code_execution_20250522"
}

const searchQueryPrefix = "perform a web search for the query:"

// NormalizeSearchQuery 去掉常见套话，只留检索词。
func NormalizeSearchQuery(query string) string {
	q := strings.TrimSpace(query)
	if len(q) >= len(searchQueryPrefix) && strings.EqualFold(q[:len(searchQueryPrefix)], searchQueryPrefix) {
		return strings.TrimSpace(q[len(searchQueryPrefix):])
	}
	return q
}

func lastTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == "text" {
			if t := strings.TrimSpace(blocks[i].Text); t != "" {
				return t
			}
		}
	}
	return ""
}
