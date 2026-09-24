package secret

import (
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	Configure("")
	if Seal("plain") != "plain" {
		t.Fatal("no key should be identity")
	}
	Configure("test-passphrase")
	t.Cleanup(func() { Configure("") })
	if !Enabled() {
		t.Fatal("expected enabled")
	}
	got := Seal("sk-secret-token")
	if !strings.HasPrefix(got, prefix) || strings.Contains(got, "sk-secret-token") {
		t.Fatalf("sealed: %s", got)
	}
	if Open(got) != "sk-secret-token" {
		t.Fatalf("open: %s", Open(got))
	}
	if Open("already-plain") != "already-plain" {
		t.Fatal("plaintext passthrough")
	}
}

func TestHexKey(t *testing.T) {
	Configure("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Cleanup(func() { Configure("") })
	if Open(Seal("hello")) != "hello" {
		t.Fatal("hex key roundtrip")
	}
}
