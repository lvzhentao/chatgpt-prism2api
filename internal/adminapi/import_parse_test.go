package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func importTestJWT(t *testing.T) string {
	t.Helper()
	return importTestJWTWithSub(t, "auth0|user_01TEST")
}

func importTestJWTWithSub(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": sub})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestParseImportTextDashLine(t *testing.T) {
	tok := importTestJWT(t)
	line := "Alice@outlook.com----pw1----pw2----user_01ABC%3A%3A" + tok
	items, err := ParseImportText(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("len=%d", len(items))
	}
	if items[0].Email != "Alice@outlook.com" {
		t.Fatalf("email=%q", items[0].Email)
	}
	if items[0].Name != "Alice" {
		t.Fatalf("name=%q", items[0].Name)
	}
	if items[0].SessionToken == "" && items[0].AccessToken == "" {
		t.Fatal("missing token fields")
	}
}

func TestParseImportJSONFlyCursor(t *testing.T) {
	tok := importTestJWT(t)
	raw := `[{"email":"bob@example.com","WorkosCursorSessionToken":"user_01BOB::` + tok + `"}]`
	items, err := ParseImportRequest([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Email != "bob@example.com" || items[0].Name != "bob" {
		t.Fatalf("%+v", items)
	}
	if items[0].SessionToken == "" {
		t.Fatal("session token not mapped")
	}
}

func TestParseImportRequestText(t *testing.T) {
	tok := importTestJWT(t)
	text := "one@x.com----user_01A::" + tok + "\n# comment\n\ntwo@x.com----" + tok
	items, err := ParseImportRequest(nil, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len=%d", len(items))
	}
	if items[0].Name != "one" || items[1].Name != "two" {
		t.Fatalf("names %q %q", items[0].Name, items[1].Name)
	}
}

func TestAssignImportNamesCollision(t *testing.T) {
	items := assignImportNames([]AccountImport{{Email: "same@a.com"}, {Email: "same@b.com"}})
	if items[0].Name != "same" || !strings.HasPrefix(items[1].Name, "same-") {
		t.Fatalf("%q %q", items[0].Name, items[1].Name)
	}
}

// fakeJWT 造一个形状合法的 JWT（header.payload.sig），用于喂 exp / email 断言。
func fakeJWT(t *testing.T, exp int64, profile map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "RS256"}) + "." + enc(profile) + ".sig"
}

// 浏览器导出的 cookie 文件必须被当成「一个账号」，而不是每 cookie 一个账号。
// 访问令牌（exp 远）与会话令牌（exp 近）靠有效期区分，账号名从 JWT 里的邮箱取。
func TestParseImportCookieExport(t *testing.T) {
	access := fakeJWT(t, 4102444800, map[string]any{
		"exp":                            4102444800,
		"https://api.openai.com/profile": map[string]any{"email": "user@example.com"},
	})
	session := fakeJWT(t, 1900000000, map[string]any{"exp": 1900000000})
	jar := `[{"domain":".openai.com","name":"prism_oai_access_token","value":"` + access + `"},
	          {"domain":".openai.com","name":"prism_session_token","value":"` + session + `"},
	          {"domain":"prism.openai.com","name":"oai-did","value":"device-uuid"}]`
	items, err := ParseImportRequest([]byte(jar), "")
	if err != nil {
		t.Fatalf("cookie jar 解析失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("cookie 导出应折叠成 1 个账号，得到 %d 个: %+v", len(items), items)
	}
	if items[0].AccessToken != access {
		t.Fatalf("应取有效期最长的令牌作访问令牌, got %.20q", items[0].AccessToken)
	}
	if items[0].SessionToken != session {
		t.Fatalf("应把短效令牌当会话令牌, got %.20q", items[0].SessionToken)
	}
	if items[0].Email != "user@example.com" {
		t.Fatalf("邮箱未从 JWT 提取: %q", items[0].Email)
	}
	if items[0].Name == "" {
		t.Fatal("导入后应有账号名（由邮箱派生）")
	}

	// 套 url 外壳的导出（Cookie-Editor "Export as JSON"）
	wrapped := `{"url":"https://prism.openai.com","cookies":[{"domain":".openai.com","name":"prism_oai_access_token","value":"` + access + `"}]}`
	items, err = ParseImportRequest([]byte(wrapped), "")
	if err != nil {
		t.Fatalf("带 url 外壳的导出解析失败: %v", err)
	}
	if len(items) != 1 || items[0].AccessToken != access {
		t.Fatalf("带外壳的 cookie 导出未识别: %+v", items)
	}

	// 会话令牌不是 JWT 时（不透明串）也要能认出来，别退回成「每个 cookie 一个账号」
	opaque := `[{"domain":".openai.com","name":"prism_session_token","value":"user_abc::opaque-session"}]`
	items, err = ParseImportRequest([]byte(opaque), "")
	if err != nil {
		t.Fatalf("不透明 session cookie 解析失败: %v", err)
	}
	if len(items) != 1 || items[0].SessionToken == "" {
		t.Fatalf("不透明 session cookie 未识别: %+v", items)
	}
}

// 账号对象数组不能被 cookie 逻辑抢走。
func TestParseImportAccountArrayNotTreatedAsCookies(t *testing.T) {
	raw := `[{"name":"a1","access_token":"eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.sig"},{"name":"a2","access_token":"tok2"}]`
	items, err := ParseImportRequest([]byte(raw), "")
	if err != nil {
		t.Fatalf("账号数组解析失败: %v", err)
	}
	if len(items) != 2 || items[0].Name != "a1" || items[1].Name != "a2" {
		t.Fatalf("账号数组被误判: %+v", items)
	}
}

// 整条 Cookie 头里会话令牌排在后面：必须按有效期选出访问令牌（10 天），
// 不能像 ExtractAccessToken 那样取最后一个（那是 12 小时的 session）。
func TestParseImportLineCookieHeaderPicksLongLivedToken(t *testing.T) {
	access := fakeJWT(t, 4102444800, map[string]any{"exp": 4102444800})
	session := fakeJWT(t, 1900000000, map[string]any{"exp": 1900000000})
	line := "prism_oai_access_token=" + access + "; prism_session_token=" + session

	in, err := ParseImportLine(line)
	if err != nil {
		t.Fatalf("Cookie 头解析失败: %v", err)
	}
	if in.AccessToken != access {
		t.Fatalf("访问令牌选错（应取有效期长的那个）: got %.30q", in.AccessToken)
	}
	if in.SessionToken != session {
		t.Fatalf("会话令牌选错: got %.30q", in.SessionToken)
	}

	// ---- 行里塞一整条 Cookie 头（粘错位置的常见形态）
	in, err = ParseImportLine("u@x.com----" + line)
	if err != nil {
		t.Fatalf("---- 行解析失败: %v", err)
	}
	if in.Email != "u@x.com" || in.AccessToken != access || in.SessionToken != session {
		t.Fatalf("---- 行解析错误: %+v", in)
	}
}

// 单个 JWT 仍原样通过（回归：不能因为按有效期排序而丢令牌）。
func TestParseImportLineSingleTokenUnchanged(t *testing.T) {
	tok := fakeJWT(t, 4102444800, map[string]any{"exp": 4102444800})
	in, err := ParseImportLine(tok)
	if err != nil {
		t.Fatalf("单 token 解析失败: %v", err)
	}
	if in.AccessToken != tok || in.SessionToken != "" {
		t.Fatalf("单 token 解析错误: %+v", in)
	}
	in, err = ParseImportLine("user_abc::opaque")
	if err != nil || in.SessionToken == "" {
		t.Fatalf("不透明 session token 解析错误: %+v err=%v", in, err)
	}
}
