package websearch

import (
	"testing"
)

func TestIsOnlyWebSearch(t *testing.T) {
	body := []byte(`{"tools":[{"type":"web_search_20250305"}],"messages":[{"role":"user","content":"latest rust release"}]}`)
	if !IsOnlyWebSearch(body) {
		t.Fatal("expected intercept")
	}
	if ExtractQuery(body) != "latest rust release" {
		t.Fatalf("query %q", ExtractQuery(body))
	}
	mixed := []byte(`{"tools":[{"type":"web_search_20250305"},{"name":"Bash"}]}`)
	if IsOnlyWebSearch(mixed) {
		t.Fatal("mixed tools should not intercept")
	}
	if !HasWebSearch(mixed) {
		t.Fatal("mixed tools still include web_search")
	}
	blocks := []byte(`{"tools":[{"type":"web_search_20250305"}],"messages":[{"role":"user","content":[{"type":"text","text":"ignore me"},{"type":"text","text":"Perform a web search for the query: AI news 2026-08-19"}]}]}`)
	if ExtractQuery(blocks) != "Perform a web search for the query: AI news 2026-08-19" {
		t.Fatalf("query %q", ExtractQuery(blocks))
	}
}

func TestParseMarkdownResults(t *testing.T) {
	text := "- [Rust](https://blog.rust-lang.org/): release notes\n- [Go](https://go.dev/): homepage"
	got := ParseMarkdownResults(text, 5)
	if len(got) != 2 || got[0].URL != "https://blog.rust-lang.org/" {
		t.Fatalf("%+v", got)
	}
}

func TestNormalizeSearchQuery(t *testing.T) {
	q := "Perform a web search for the query: AI news 2026-08-19"
	if got := NormalizeSearchQuery(q); got != "AI news 2026-08-19" {
		t.Fatalf("normalized %q", got)
	}
}

func TestIsOnlyCodeExecution(t *testing.T) {
	if !IsOnlyCodeExecution([]byte(`{"tools":[{"name":"code_execution","type":"code_execution_20250522"}]}`)) {
		t.Fatal("expected code execution")
	}
}
