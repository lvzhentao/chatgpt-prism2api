package emulation

import "testing"

func TestExtractCodeExecutionHELLOCHECK(t *testing.T) {
	stdout, code := ExtractCodeExecution("Write and execute a Python script that prints 'HELLO_CHECK'. Only use the code execution tool, nothing else.")
	if stdout != "HELLO_CHECK\n" {
		t.Fatalf("stdout %q", stdout)
	}
	if code != `print("HELLO_CHECK")` {
		t.Fatalf("code %q", code)
	}
}
