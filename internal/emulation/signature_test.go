package emulation

import (
	"crypto/elliptic"
	"encoding/base64"
	"math/big"
	"regexp"
	"strings"
	"testing"
)

func TestThinkingSignatureShape(t *testing.T) {
	sig := ThinkingSignature("the model considers the problem", "claude-opus-4-8", "msg_01test")
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !strings.HasPrefix(sig, "E") {
		t.Fatalf("opus-4-8 signatures start with E, got %q", sig[:1])
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 200 {
		t.Fatalf("signature too short for channel-16 schema: %d", len(raw))
	}
	if raw[0] != 0x12 {
		t.Fatalf("envelope tag %x", raw[0])
	}
	info := InspectThinkingSignature(sig)
	if !info.HasChannel || info.ChannelID != officialClaudeChannelID {
		t.Fatalf("channel %+v", info)
	}
	if info.Model != "claude-opus-4-8" {
		t.Fatalf("model %q", info.Model)
	}
	if info.BlockKind != officialThinkingBlockKind {
		t.Fatalf("block kind %q", info.BlockKind)
	}
	if !canonicalUUIDRe.MatchString(info.ContextID) {
		t.Fatalf("context id %q", info.ContextID)
	}
	container, ok := protobufBytesField(raw, 2)
	if !ok {
		t.Fatal("missing container")
	}
	channel, ok := protobufBytesField(container, 1)
	if !ok {
		t.Fatal("missing channel block")
	}
	ecdsaRaw, ok := protobufBytesField(channel, 5)
	if !ok || len(ecdsaRaw) != 64 {
		t.Fatalf("channel field 5 want 64-byte ECDSA, got %d ok=%v", len(ecdsaRaw), ok)
	}
	assertP256Scalars(t, ecdsaRaw)
	if v, ok := protobufVarintField(channel, 7); !ok || v != 1 {
		t.Fatalf("channel field 7 want 1, got %d ok=%v", v, ok)
	}
	meta, ok := protobufBytesField(container, 5)
	if !ok || len(meta) != officialContainerMetaLen {
		t.Fatalf("container field 5 want %d-byte official metadata, got %d ok=%v", officialContainerMetaLen, len(meta), ok)
	}
	if len(meta) == 32 {
		t.Fatal("32-byte container field 5 is the sub2api HMAC fingerprint")
	}
	again := ThinkingSignature("the model considers the problem", "claude-opus-4-8", "msg_01test")
	if again != sig {
		t.Fatal("signature should be cached")
	}
}

var canonicalUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func assertP256Scalars(t *testing.T, raw []byte) {
	t.Helper()
	if len(raw) != 64 {
		t.Fatalf("ecdsa len %d", len(raw))
	}
	n := elliptic.P256().Params().N
	r := new(big.Int).SetBytes(raw[:32])
	s := new(big.Int).SetBytes(raw[32:])
	if r.Sign() <= 0 || s.Sign() <= 0 || r.Cmp(n) >= 0 || s.Cmp(n) >= 0 {
		t.Fatalf("field 5 is not a P-256 scalar pair r=%s s=%s", r.Text(16), s.Text(16))
	}
	half := new(big.Int).Rsh(new(big.Int).Set(n), 1)
	if s.Cmp(half) == 1 {
		t.Fatal("ECDSA s is not low-S normalized")
	}
}
