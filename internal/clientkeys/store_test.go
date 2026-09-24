package clientkeys

import (
	"strings"
	"testing"

	"prism-2api/internal/persist"
)

func TestVerifyConstantTime(t *testing.T) {
	s := New()
	e := s.Create("probe", "", "")
	if !strings.HasPrefix(e.Key, Prefix) {
		t.Fatalf("key prefix: %s", e.Key)
	}
	got, ok := s.VerifyAndTouch(e.Key)
	if !ok || got.ID != e.ID {
		t.Fatalf("expected hit id=%d, got %+v ok=%v", e.ID, got, ok)
	}
	// 同长度错误明文：Equal 走完整 ConstantTimeCompare
	miss := Prefix + strings.Repeat("x", 32)
	if _, ok := s.VerifyAndTouch(miss); ok {
		t.Fatal("same-length mismatch must miss")
	}
	if !Equal(e.Key, e.Key) {
		t.Fatal("Equal same")
	}
	if Equal(e.Key, miss) {
		t.Fatal("Equal different")
	}
	if Equal("short", "longer-key") {
		t.Fatal("Equal different lengths")
	}
}

func TestCreateAndVerify(t *testing.T) {
	s := New()
	e := s.Create("test", "desc", "team-a")
	got, ok := s.VerifyAndTouch(e.Key)
	if !ok || got.ID != e.ID || got.Group != "team-a" {
		t.Fatalf("verify: %+v ok=%v", got, ok)
	}
	if _, ok := s.VerifyAndTouch("nope"); ok {
		t.Fatal("unknown key")
	}
}

func TestDisabledKeyRejected(t *testing.T) {
	s := New()
	e := s.Create("test", "", "")
	s.SetDisabled(e.ID, true)
	if _, ok := s.VerifyAndTouch(e.Key); ok {
		t.Fatal("disabled must reject")
	}
	s.SetDisabled(e.ID, false)
	if _, ok := s.VerifyAndTouch(e.Key); !ok {
		t.Fatal("re-enabled must accept")
	}
}

func TestEnsureSystemKeyUsesIDZero(t *testing.T) {
	s := New()
	s.EnsureSystemKey("系统密钥", "", "sk-master")
	if !s.IsSystem(0) {
		t.Fatal("id=0 must be system")
	}
	if s.List()[0].ID != 0 {
		t.Fatal("first listed id")
	}
	s.EnsureSystemKey("系统密钥", "", "sk-master")
	n := 0
	for _, k := range s.List() {
		if k.IsSystem {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("idempotent system count=%d", n)
	}
	got, ok := s.VerifyAndTouch("sk-master")
	if !ok || got.ID != 0 {
		t.Fatalf("system verify: %+v ok=%v", got, ok)
	}
}

func TestEnsureSystemKeyMigratesMisplacedID(t *testing.T) {
	s := New()
	s.CreateWithKey("系统密钥", "", "", "sk-master")
	if s.List()[0].ID != 1 {
		t.Fatal("bootstrap at id=1")
	}
	s.EnsureSystemKey("系统密钥", "", "sk-master")
	if !s.IsSystem(0) {
		t.Fatal("migrated to id=0")
	}
	for _, k := range s.List() {
		if k.ID == 1 && k.Key == "sk-master" {
			t.Fatal("old id=1 still holds plaintext")
		}
	}
}

func TestSystemKeyCannotBeDeleted(t *testing.T) {
	s := New()
	s.EnsureSystemKey("系统密钥", "", "sk-master")
	if s.Delete(0) {
		t.Fatal("system key must not delete")
	}
	if !s.IsSystem(0) {
		t.Fatal("still system")
	}
}

func TestMaskHidesShortSystemKey(t *testing.T) {
	if Mask("sk-test-key") == "sk-test-key" {
		t.Fatal("short system key must be masked")
	}
	if got, want := Mask("csk_abcdefghijklmnop"), "csk_abcd...mnop"; got != want {
		t.Fatalf("mask=%s want=%s", got, want)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	mem := persist.NewMemory()
	s, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Create("disk", "", "g")
	s.Flush()
	s2, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.VerifyAndTouch(e.Key)
	if !ok || got.Group != "g" {
		t.Fatalf("reloaded: %+v ok=%v", got, ok)
	}
}
