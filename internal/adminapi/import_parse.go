package adminapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"prism-2api/internal/auth"
	"prism-2api/internal/pool"
)

var errEmptyImportLine = errors.New("empty import line")

// ParseImportRequest 解析管理端导入体：accounts JSON（对象/字符串混排）或 text（---- 行）。
func ParseImportRequest(accounts json.RawMessage, text string) ([]AccountImport, error) {
	text = strings.TrimSpace(text)
	if text != "" {
		return ParseImportText(text)
	}
	if len(bytes.TrimSpace(accounts)) == 0 {
		return nil, fmt.Errorf("accounts or text is required")
	}
	items, err := parseImportJSON(accounts)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("accounts is required")
	}
	return assignImportNames(items), nil
}

// ParseImportText 解析导入文本：JSON 数组，或
// email----password----…----token 逐行。
func ParseImportText(text string) ([]AccountImport, error) {
	items, err := parseImportText(text)
	if err != nil {
		return nil, err
	}
	return assignImportNames(items), nil
}

func parseImportText(text string) ([]AccountImport, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty import text")
	}
	if text[0] == '{' || text[0] == '[' {
		return parseImportJSON([]byte(text))
	}
	var items []AccountImport
	for i, line := range strings.Split(text, "\n") {
		in, err := ParseImportLine(line)
		if err != nil {
			if errors.Is(err, errEmptyImportLine) {
				continue
			}
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		items = append(items, in)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no accounts found")
	}
	return items, nil
}

// ParseImportLine 解析单行：JSON 对象，或 ---- 分隔，或纯 token（含整条 Cookie 头）。
func ParseImportLine(line string) (AccountImport, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return AccountImport{}, errEmptyImportLine
	}
	if strings.HasPrefix(line, "{") {
		var in AccountImport
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			return AccountImport{}, err
		}
		return in, nil
	}
	var in AccountImport
	parts := strings.Split(line, "----")
	if len(parts) == 1 {
		access, session := pickTokens(line)
		if access == "" && session == "" {
			return AccountImport{}, fmt.Errorf("no session token")
		}
		in.AccessToken, in.SessionToken = access, session
		return in, nil
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		access, session := pickTokens(p)
		if access != "" || session != "" {
			if access != "" && in.AccessToken == "" {
				in.AccessToken = access
			}
			if session != "" && in.SessionToken == "" {
				in.SessionToken = session
			}
			continue
		}
		if strings.Contains(p, "@") && in.Email == "" {
			in.Email = p
		}
	}
	if in.AccessToken == "" && in.SessionToken == "" {
		return AccountImport{}, fmt.Errorf("no session token")
	}
	return in, nil
}

// pickTokens 从一段文本里挑出访问令牌与会话令牌。
//
// 一段文本常同时含两个令牌（整条 Cookie 头、Cookie-Editor 的两行），
// `auth.ExtractAccessToken` 只取最后一个 —— 而 Prism 的 Cookie 头里会话令牌排在后面，
// 直接取末位会把 12 小时的 session 当 10 天的 access 用。这里按 JWT 的有效期分：
// 活得久的那个是访问令牌。
func pickTokens(raw string) (access, session string) {
	tokens := auth.ExtractTokens(raw)
	if len(tokens) == 0 {
		if looksLikeSessionToken(raw) {
			return "", strings.TrimSpace(raw)
		}
		return "", ""
	}
	if len(tokens) == 1 {
		return tokens[0], ""
	}
	sort.SliceStable(tokens, func(i, j int) bool {
		return auth.JWTExpiry(tokens[i]) > auth.JWTExpiry(tokens[j])
	})
	return tokens[0], tokens[1]
}

func looksLikeSessionToken(p string) bool {
	return strings.HasPrefix(p, "user_") || strings.Contains(p, "::") || strings.Contains(strings.ToLower(p), "%3a%3a")
}

func parseImportJSON(raw []byte) ([]AccountImport, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty json")
	}
	if raw[0] == '[' {
		return parseImportArray(raw)
	}
	if raw[0] != '{' {
		return nil, fmt.Errorf("invalid json")
	}
	var cookieWrap struct {
		Cookies json.RawMessage `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &cookieWrap); err == nil && len(bytes.TrimSpace(cookieWrap.Cookies)) > 0 {
		if in, ok := parseCookieJar(cookieWrap.Cookies); ok {
			return []AccountImport{in}, nil
		}
	}
	var wrap struct {
		Accounts json.RawMessage `json:"accounts"`
		Text     string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && (len(bytes.TrimSpace(wrap.Accounts)) > 0 || strings.TrimSpace(wrap.Text) != "") {
		if t := strings.TrimSpace(wrap.Text); t != "" {
			return parseImportText(t)
		}
		return parseImportArray(wrap.Accounts)
	}
	if in, ok := parseCookieJar(raw); ok {
		return []AccountImport{in}, nil
	}
	var one AccountImport
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []AccountImport{one}, nil
}

func parseImportArray(raw []byte) ([]AccountImport, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	if in, ok := parseCookieJar(raw); ok {
		return []AccountImport{in}, nil
	}
	out := make([]AccountImport, 0, len(elems))
	for i, el := range elems {
		el = bytes.TrimSpace(el)
		if len(el) == 0 {
			continue
		}
		if el[0] == '"' {
			var s string
			if err := json.Unmarshal(el, &s); err != nil {
				return nil, fmt.Errorf("item %d: %w", i+1, err)
			}
			in, err := ParseImportLine(s)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i+1, err)
			}
			out = append(out, in)
			continue
		}
		var in AccountImport
		if err := json.Unmarshal(el, &in); err != nil {
			return nil, fmt.Errorf("item %d: %w", i+1, err)
		}
		out = append(out, in)
	}
	return out, nil
}

func assignImportNames(items []AccountImport) []AccountImport {
	used := map[string]int{}
	for i := range items {
		name := strings.TrimSpace(items[i].Name)
		if name == "" {
			name = pool.SanitizeName(items[i].Email)
		} else if !pool.ValidName(name) {
			name = pool.SanitizeName(name)
		}
		if name == "" {
			name = "acc"
		}
		n := used[strings.ToLower(name)]
		used[strings.ToLower(name)]++
		if n > 0 {
			name = numberedName(name, n+1)
		}
		items[i].Name = name
	}
	return items
}

func numberedName(base string, n int) string {
	suf := fmt.Sprintf("-%d", n)
	if len(base)+len(suf) > 32 {
		base = base[:32-len(suf)]
	}
	return base + suf
}

// cookieExportEntry 浏览器 cookie 导出（Cookie-Editor / EditThisCookie / Playwright
// storage_state）里的一条 cookie。
type cookieExportEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// parseCookieJar 把「浏览器导出的 cookie 列表」识别成**一个**账号。
//
// 导出文件本身不含账号语义，只有一堆 cookie；令牌靠 JWT 的 exp 区分：
// 访问令牌活得久（Prism 的 oai token 10 天），会话令牌短（12 小时）。
// 返回 ok=false 表示这堆条目不是 cookie 导出（可能是账号对象数组），交给原路径处理。
func parseCookieJar(raw json.RawMessage) (AccountImport, bool) {
	var entries []cookieExportEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return AccountImport{}, false
	}
	shaped, domained := 0, 0
	type candidate struct {
		token string
		exp   int64
	}
	var candidates []candidate
	var sessionLike string
	for _, e := range entries {
		name, value := strings.TrimSpace(e.Name), strings.TrimSpace(e.Value)
		if name == "" || value == "" {
			continue
		}
		shaped++
		if strings.TrimSpace(e.Domain) != "" {
			domained++
		}
		if tok := auth.ExtractAccessToken(value); tok != "" {
			candidates = append(candidates, candidate{token: tok, exp: auth.JWTExpiry(tok)})
			continue
		}
		if sessionLike == "" && looksLikeSessionToken(value) {
			sessionLike = value
		}
	}
	// 每条都是 name/value、且至少一条带 domain —— 才认定是 cookie 导出；
	// 账号对象数组（name + access_token）不满足这个形状。
	if shaped != len(entries) || domained == 0 {
		return AccountImport{}, false
	}
	if len(candidates) == 0 {
		if sessionLike == "" {
			return AccountImport{}, false
		}
		return AccountImport{SessionToken: sessionLike}, true
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].exp > candidates[j].exp })
	in := AccountImport{
		AccessToken: candidates[0].token,
		Email:       auth.JWTEmail(candidates[0].token),
	}
	if len(candidates) > 1 {
		in.SessionToken = candidates[1].token
	}
	return in, true
}
