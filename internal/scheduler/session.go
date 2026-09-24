package scheduler

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// CPA sdk/cliproxy/session/identity.go + navos extractModelSessionId。
var (
	cpaClaudeSessionRe   = regexp.MustCompile(`_session_([a-f0-9-]+)$`)
	navosClaudeSessionRe = regexp.MustCompile(`(?:^|_)session_([A-Za-z0-9-]+)`)
)

// ExtractSessionIDs 返回 (primary, fallback)。
// 顺序来自 CPA extractSessionIDs；Claude user_id 同时接受 CPA 后缀与 navos session_ 片段。
func ExtractSessionIDs(headers http.Header, payload []byte) (string, string) {
	if sid := headerValue(headers, "X-Claude-Code-Session-Id"); sid != "" {
		return "claude:" + sid, ""
	}
	if sid := claudeMetadataSessionID(payload); sid != "" {
		return "claude:" + sid, ""
	}
	if sid := headerValue(headers, "Session-Id"); sid != "" {
		return "codex:" + sid, ""
	}
	if sid := headerValue(headers, "Session_id"); sid != "" {
		return "codex:" + sid, ""
	}
	if sid := headerValue(headers, "X-Session-ID"); sid != "" {
		return "header:" + sid, ""
	}
	if sid := headerValue(headers, "X-Session-Affinity"); sid != "" {
		return "affinity:" + sid, ""
	}
	if sid := headerValue(headers, "X-Client-Request-Id"); sid != "" {
		return "clientreq:" + sid, ""
	}

	if len(payload) > 0 {
		for _, path := range []string{"session_id", "sessionId"} {
			if sid := normalizeID(jsonPathString(payload, path)); sid != "" {
				return "session:" + sid, ""
			}
		}
		conversationID := ""
		if sid := normalizeID(jsonPathString(payload, "conversation", "id")); sid != "" {
			conversationID = "conv:" + sid
		} else if sid := normalizeID(jsonPathString(payload, "conversation")); sid != "" {
			conversationID = "conv:" + sid
		}
		if sid := normalizeID(jsonPathString(payload, "prompt_cache_key")); sid != "" {
			return "pck:" + sid, conversationID
		}
		if conversationID != "" {
			return conversationID, ""
		}
		if userID := normalizeID(jsonPathString(payload, "metadata", "user_id")); userID != "" {
			return "user:" + userID, ""
		}
		if sid := normalizeID(jsonPathString(payload, "conversation_id")); sid != "" {
			return "conv:" + sid, ""
		}
	}
	return "", ""
}

func claudeMetadataSessionID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	userID := strings.TrimSpace(jsonPathString(payload, "metadata", "user_id"))
	if userID == "" {
		return ""
	}
	if strings.HasPrefix(userID, "{") {
		return normalizeID(jsonPathString([]byte(userID), "session_id"))
	}
	if m := cpaClaudeSessionRe.FindStringSubmatch(userID); len(m) >= 2 {
		return normalizeID(m[1])
	}
	if m := navosClaudeSessionRe.FindStringSubmatch(userID); len(m) >= 2 {
		return normalizeID(m[1])
	}
	return ""
}

func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if v := normalizeID(headers.Get(name)); v != "" {
		return v
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, raw := range values {
			if v := normalizeID(raw); v != "" {
				return v
			}
		}
	}
	return ""
}

func normalizeID(raw string) string {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	return raw
}

func jsonPathString(body []byte, path ...string) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	cur := v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			if p == path[len(path)-1] {
				if s, ok := cur.(string); ok {
					return s
				}
			}
			return ""
		}
		cur, ok = m[p]
		if !ok {
			return ""
		}
	}
	switch t := cur.(type) {
	case string:
		return t
	default:
		return ""
	}
}
