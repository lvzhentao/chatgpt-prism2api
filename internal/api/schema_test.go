package api

import (
	"encoding/json"
	"testing"
)

func TestNormalizeJSONSchema(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "required null becomes empty array",
			in:   `{"type":"object","properties":{"a":{"type":"string"}},"required":null}`,
			want: map[string]any{"type": "object", "required": []any{}, "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		},
		{
			name: "missing type and properties get defaults",
			in:   `{"description":"x"}`,
			want: map[string]any{"description": "x", "type": "object", "properties": map[string]any{}, "required": []any{}},
		},
		{
			name: "illegal type and properties values corrected",
			in:   `{"type":123,"properties":null,"required":"oops"}`,
			want: map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}},
		},
		{
			name: "valid schema preserved",
			in:   `{"type":"object","properties":{"b":{"type":"number"}},"required":["b"]}`,
			want: map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "number"}}, "required": []any{"b"}},
		},
		{
			name: "empty schema string removed, non-bool additionalProperties corrected",
			in:   `{"$schema":"","type":"object","additionalProperties":123}`,
			want: map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}, "additionalProperties": true},
		},
		{
			name: "null raw passthrough",
			in:   `null`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := normalizeJSONSchema(json.RawMessage(c.in))
			if c.want == nil {
				if string(out) != `null` {
					t.Fatalf("null input: got %s", out)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal normalized: %v (raw=%s)", err, out)
			}
			wantJSON, _ := json.Marshal(c.want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("normalized mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
