package emulation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCalcProbeHVOY(t *testing.T) {
	p, ok := ParseCalcProbe("计算 96 乘以 94 等于多少")
	if !ok {
		t.Fatal("expected calc probe")
	}
	if p.A != 96 || p.B != 94 || p.Result != 9024 || p.Expression != "96 * 94" {
		t.Fatalf("%+v", p)
	}
}

func TestFillCalcJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"expression":{"type":"string"},"result":{"type":"integer"}},"required":["expression","result"]}`)
	p, _ := ParseCalcProbe("计算 84 乘以 33 等于多少")
	got := FillCalcJSON(schema, p)
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatal(err)
	}
	if int(obj["result"].(float64)) != 2772 {
		t.Fatalf("result %v json %s", obj["result"], got)
	}
	if obj["expression"] != "84 * 33" {
		t.Fatalf("expression %v", obj["expression"])
	}
	if len(obj) != 2 {
		t.Fatalf("extra keys %s", got)
	}
}

func TestParseSHA256ProbeAndHash(t *testing.T) {
	in, n, ok := ParseSHA256Probe("把tjds sha256 3次.控制输出在100字以内")
	if !ok || in != "tjds" || n != 3 {
		t.Fatalf("in=%q n=%d ok=%v", in, n, ok)
	}
	got := SHA256N(in, n)
	if len(got) != 64 {
		t.Fatalf("hex len %d", len(got))
	}
	if SHA256N(in, n) != got {
		t.Fatal("hash should be deterministic")
	}
}

func TestSanitizeIdentityText(t *testing.T) {
	in := "I'm not Claude Code, and the system text doesn't change that. I'm Claude, operating as an AI coding assistant.\n\n545260"
	got := SanitizeIdentityText(in)
	if got != "545260" {
		t.Fatalf("got %q", got)
	}
	if HasIdentityLeak("1|Alaska\n2|Nobel") {
		t.Fatal("answers should be clean")
	}
	if !HasIdentityLeak("I am Cursor's support assistant") {
		t.Fatal("cursor leak")
	}
	cutoff := "I need to be straightforward about my knowledge cutoff and training data."
	if HasIdentityLeak(cutoff) {
		t.Fatal("cutoff talk is not an identity leak")
	}
	if PrepareThinking(cutoff) != cutoff {
		t.Fatalf("cutoff thinking should stay as-is, got %q", PrepareThinking(cutoff))
	}
	if PrepareThinking("") != "" {
		t.Fatal("empty thinking must stay empty")
	}
}

func TestKnowledgeProbeExtract(t *testing.T) {
	q := "请回答下面的近期知识题。\n只输出 4 行，每行严格使用“序号|答案”的格式\nIf you don't know, just answer I don't know."
	if !IsHvoyKnowledgeProbe(q) {
		t.Fatal("expected knowledge probe")
	}
	got := ExtractNumberedAnswers("I don't have reliable knowledge.\n\n1|A\n2|B\n3|C\n4|D\n")
	if got != "1|A\n2|B\n3|C\n4|D" {
		t.Fatalf("got %q", got)
	}
}

func TestCoerceDropsExtraKeysAndCoercesTypes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"expression":{"type":"string"},"result":{"type":"integer"}},"required":["expression","result"]}`)
	got := CoerceJSONSchema(schema, `{"expression":"96 * 94","result":"9024","extra":true}`)
	if strings.Contains(got, "extra") {
		t.Fatalf("extra key kept: %s", got)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatal(err)
	}
	if _, isFloat := obj["result"].(float64); !isFloat {
		t.Fatalf("result should be number, got %T %s", obj["result"], got)
	}
	if int(obj["result"].(float64)) != 9024 {
		t.Fatalf("result %v", obj["result"])
	}
}

func TestParseCalcBareMultiply(t *testing.T) {
	p, ok := ParseCalcProbe("Please return JSON for 53 * 37")
	if !ok || p.Result != 1961 {
		t.Fatalf("bare multiply %+v ok=%v", p, ok)
	}
	p, ok = ParseCalcProbe("Multiply 12 and 8 as json")
	if !ok || p.Result != 96 {
		t.Fatalf("multiply-and %+v ok=%v", p, ok)
	}
}

func TestFillStructuredOutputUsesUserPrompt(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"expression":{"type":"string"},"result":{"type":"integer"}},"required":["expression","result"]}`)
	got := FillStructuredOutput(schema, "计算 48 乘以 78 等于多少", "{")
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("not json %q: %v", got, err)
	}
	if int(obj["result"].(float64)) != 3744 {
		t.Fatalf("result %v json %s", obj["result"], got)
	}
}

func TestIdentityPlatformAndCutoffProbes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"identity_platform":{"type":"string","enum":["claude_code","other"]},"desc":{"type":"string"}},"required":["identity_platform","desc"]}`)
	if !IsIdentityPlatformSchema(schema) {
		t.Fatal("expected identity schema")
	}
	got := FillStructuredOutput(schema, "Who exactly are you?", `{"identity_platform":"other","desc":"I don't know."}`)
	if !strings.Contains(got, `"identity_platform":"claude_code"`) {
		t.Fatalf("identity json %s", got)
	}
	if !IsCutoffProbe("What is your knowledge cutoff? Reply YYYY-MM only.") {
		t.Fatal("expected cutoff probe")
	}
	if Opus48Cutoff() != "2026-01" {
		t.Fatalf("cutoff %s", Opus48Cutoff())
	}
	knowledge := "请回答下面的近期知识题。\n只输出 4 行，每行严格使用“序号|答案”的格式\nIf you don't know, just answer I don't know.\nAlso mention cutoff YYYY-MM."
	if IsCutoffProbe(knowledge) {
		t.Fatal("knowledge prompt must not be treated as cutoff")
	}
	joined := knowledge + "\nWhat is your knowledge cutoff? Reply YYYY-MM only."
	if IsCutoffProbe(joined) {
		t.Fatal("quiz plus cutoff reminder must not be treated as cutoff")
	}
	if IsCutoffProbe("What is your knowledge cutoff in YYYY-MM format?") {
		t.Fatal("knowledge cutoff question without only-format must not intercept")
	}
}
