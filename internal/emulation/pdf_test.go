package emulation

import (
	"strings"
	"testing"
)

func TestExtractPDFTextFindsLiteral(t *testing.T) {
	pdf := []byte("%PDF-1.1\n1 0 obj\n<<>>\nendobj\nBT (HELLO_CHECK 438810) Tj ET\n")
	got := ExtractPDFText(pdf)
	if !strings.Contains(got, "HELLO_CHECK") || !strings.Contains(got, "438810") {
		t.Fatalf("extracted %q", got)
	}
}

func TestExtractHVOYPDFToken(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 80] /Contents 4 0 R >>\nendobj\n4 0 obj\n<< /Length 57 >>\nstream\nBT /F1 14 Tf 10 20 Td (Hvoy AI report total 438810) Tj ET\nendstream\nendobj\n")
	text := ExtractPDFText(pdf)
	if !strings.Contains(text, "438810") || !strings.Contains(text, "Hvoy") {
		t.Fatalf("extracted %q", text)
	}
	if got := HvoyReportTotal(text); got != "438810" {
		t.Fatalf("token %q from %q", got, text)
	}
}

func TestExtractDocumentTextSource(t *testing.T) {
	got := ExtractDocument("text/plain", "text", "plain body", "", "note")
	if !strings.Contains(got, "plain body") {
		t.Fatalf("got %q", got)
	}
}
