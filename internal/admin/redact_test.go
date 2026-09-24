package admin

import (
	"strings"
	"testing"
)

func TestRedactBodySecretsAndTruncate(t *testing.T) {
	huge := strings.Repeat("x", 9000)
	out, ok := RedactBody(map[string]any{
		"username":      "admin",
		"password":      "admin123",
		"token":         "abcdef123456",
		"api_key":       "sk-live-secret",
		"refresh_token": "rt-keep-hidden",
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
		"note":          huge,
	}).(map[string]any)
	if !ok {
		t.Fatal("expected map")
	}
	if out["username"] != "admin" {
		t.Fatalf("username: %v", out["username"])
	}
	if out["password"] != "[REDACTED]" {
		t.Fatalf("password: %v", out["password"])
	}
	if out["refresh_token"] != "[REDACTED]" {
		t.Fatalf("refresh_token: %v", out["refresh_token"])
	}
	tok, _ := out["token"].(string)
	if strings.Contains(tok, "abcdef123456") || !strings.Contains(tok, "***") {
		t.Fatalf("token not masked: %v", out["token"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "[truncated") {
		t.Fatalf("long string not truncated: %s", note[:32])
	}
	if !strings.HasSuffix(note, strings.Repeat("x", 32)) && !strings.Contains(note[len(note)-20:], "x") {
		t.Fatalf("truncated string should keep tail: %s", note[len(note)-40:])
	}
}

func TestTruncateHeadTailKeepsMarker(t *testing.T) {
	got := truncateHeadTail("HEAD"+strings.Repeat("m", 200)+"TAIL", 40)
	if !strings.Contains(got, "HEAD") || !strings.Contains(got, "TAIL") || !strings.Contains(got, "[truncated") {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeHeaders(t *testing.T) {
	got := SanitizeHeaders(map[string][]string{
		"Authorization": {"Bearer super-secret-token"},
		"X-Api-Key":     {"csk_abcdefghijklmnopqrstuvwxyz"},
		"Cookie":        {"session=abc123xyz"},
		"Content-Type":  {"application/json"},
	})
	if !strings.Contains(got["authorization"], "Bearer") || strings.Contains(got["authorization"], "super-secret-token") {
		t.Fatalf("authorization: %q", got["authorization"])
	}
	if strings.Contains(got["x-api-key"], "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("x-api-key leaked: %q", got["x-api-key"])
	}
	if strings.Contains(got["cookie"], "abc123xyz") {
		t.Fatalf("cookie leaked: %q", got["cookie"])
	}
	if got["content-type"] != "application/json" {
		t.Fatalf("content-type: %q", got["content-type"])
	}
}

func TestRedactRequestBodyAndText(t *testing.T) {
	body := RedactRequestBody([]byte(`{"password":"admin123","token":"abc12345"}`))
	if strings.Contains(body, "admin123") {
		t.Fatalf("password leaked: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected redacted password: %s", body)
	}
	txt := RedactText("failed Bearer sk-live-abcdefgh Authorization api_key=csk_abcdefg")
	if strings.Contains(txt, "sk-live-abcdefgh") || strings.Contains(txt, "csk_abcdefg") {
		t.Fatalf("text leaked: %s", txt)
	}
}

func TestSafeStringifyCaps(t *testing.T) {
	raw := "HEAD" + strings.Repeat("m", 100) + "TAIL"
	got := SafeStringify(raw, 40)
	if !strings.Contains(got, "[truncated") || !strings.Contains(got, "HEAD") || !strings.Contains(got, "TAIL") {
		t.Fatalf("expected head+tail truncation: %s", got)
	}
}
