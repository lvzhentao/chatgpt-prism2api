package emulation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoerceJSONSchemaHVOYCalc(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string"},"result":{"type":"integer"}},"required":["expression","result"]}`)
	got := CoerceJSONSchema(schema, "48 × 78 = 3744")
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("not json %q: %v", got, err)
	}
	if int(obj["result"].(float64)) != 3744 {
		t.Fatalf("result %v", obj["result"])
	}
	expr, _ := obj["expression"].(string)
	if !strings.Contains(expr, "48") || !strings.Contains(expr, "78") {
		t.Fatalf("expression %q", expr)
	}
}

func TestCoerceJSONSchemaKeepsValidObject(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"result":{"type":"integer"}}}`)
	got := CoerceJSONSchema(schema, `{"result": 9}`)
	if got != `{"result": 9}` && got != `{"result":9}` {
		var obj map[string]any
		if err := json.Unmarshal([]byte(got), &obj); err != nil || int(obj["result"].(float64)) != 9 {
			t.Fatalf("got %q", got)
		}
	}
}
