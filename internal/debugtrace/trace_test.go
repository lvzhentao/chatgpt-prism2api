package debugtrace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	Configure(dir)
	t.Cleanup(func() { Configure("") })

	s := Start("req-1", "POST", "/v1/messages", "", map[string]string{"authorization": "Bearer ***"}, []byte(`{"model":"opus"}`))
	if s == nil {
		t.Fatal("Start returned nil")
	}
	ctx := With(context.Background(), s)
	Emit(ctx, "thinking_delta", map[string]any{"text": "hmm"})
	s.Finish(map[string]any{"status": 200})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("files: %d", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%d body=%s", len(lines), raw)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["kind"] != "http_request" {
		t.Fatalf("first kind %v", first["kind"])
	}
}

func TestNilSessionIsNoop(t *testing.T) {
	Configure("")
	var s *Session
	s.Emit("x", 1)
	s.Finish(nil)
	Emit(context.Background(), "x", 1)
}
