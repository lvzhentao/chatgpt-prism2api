package auth

import (
	"net/url"
	"regexp"
	"strings"
)

// jwtRE 匹配三段 JWT（header/payload 都以 eyJ 开头）。
var jwtRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+={0,2}\.eyJ[A-Za-z0-9_-]+={0,2}\.[A-Za-z0-9_-]+={0,2}`)

// ExtractAccessToken 从 JWT、WorkosCursorSessionToken 或整段导入文本里抽出 access JWT。
func ExtractAccessToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
		raw = strings.TrimSpace(dec)
	}
	if i := strings.Index(raw, "WorkosCursorSessionToken="); i >= 0 {
		raw = raw[i+len("WorkosCursorSessionToken="):]
		if j := strings.IndexAny(raw, "; \t\r\n"); j >= 0 {
			raw = raw[:j]
		}
		if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
			raw = dec
		}
	}
	if i := strings.Index(raw, "::"); i >= 0 {
		raw = raw[i+2:]
	}
	matches := jwtRE.FindAllString(raw, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// ExtractTokens 返回文本中所有 JWT 形状的令牌，按出现顺序去重。
// 一段文本里可能同时含访问令牌与会话令牌（如整条 Cookie 头），
// ExtractAccessToken 只取最后一个，不一定是要的那个。
func ExtractTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
		raw = dec
	}
	matches := jwtRE.FindAllString(raw, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// WorkOSUserID 从 access JWT 的 sub 取出 Cursor WorkOS user_…。
func WorkOSUserID(accessToken string) string {
	sub := JWTSubject(accessToken)
	if sub == "" {
		return ""
	}
	if i := strings.LastIndex(sub, "|"); i >= 0 {
		sub = sub[i+1:]
	}
	sub = strings.TrimSpace(sub)
	if strings.HasPrefix(sub, "user_") {
		return sub
	}
	return ""
}
