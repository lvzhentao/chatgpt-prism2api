package emulation

import (
	"strings"
	"testing"
)

func TestPrefixedIDs(t *testing.T) {
	if got := MessageID(); !strings.HasPrefix(got, "msg_01") || len(got) != 6+idSuffixN {
		t.Fatalf("MessageID %q", got)
	}
	if got := RequestID(); !strings.HasPrefix(got, "req_01") || len(got) != 6+idSuffixN {
		t.Fatalf("RequestID %q", got)
	}
	if got := ToolID(); !strings.HasPrefix(got, "toolu_01") || len(got) != 8+idSuffixN {
		t.Fatalf("ToolID %q", got)
	}
	if got := ServerToolID(); !strings.HasPrefix(got, "srvtoolu_01") {
		t.Fatalf("ServerToolID %q", got)
	}
	if !IsServerToolID("srvtoolu_01abc") || IsServerToolID("toolu_01abc") {
		t.Fatal("IsServerToolID")
	}
}
