package prism

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B 官方向量（SHA1），secret = ASCII "12345678901234567890"。
// 8 位码是标准向量，这里取后 6 位（我们只用 6 位）。
func TestTOTPCodeRFC6238Vectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32("12345678901234567890")
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, tc := range cases {
		got, err := totpCode(secret, time.Unix(tc.unix, 0))
		if err != nil {
			t.Fatalf("totpCode(%d): %v", tc.unix, err)
		}
		if got != tc.want {
			t.Fatalf("totpCode(%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}

// 用户粘进来的 secret 常见形态：小写、带空格、缺 padding。
func TestTOTPCodeAcceptsMessySecret(t *testing.T) {
	at := time.Unix(1111111109, 0)
	for _, secret := range []string{
		"gezdgnbvgy3tqojqgezdgnbvgy3tqojq",
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
	} {
		got, err := totpCode(secret, at)
		if err != nil {
			t.Fatalf("totpCode(%q): %v", secret, err)
		}
		if got != "081804" {
			t.Fatalf("totpCode(%q) = %s, want 081804", secret, got)
		}
	}
}

func TestVerifyTOTPWindowAndRejects(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	at := time.Unix(1111111109, 0)
	current, err := totpCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyTOTP(secret, current, at) {
		t.Fatal("current code must verify")
	}
	// 时钟漂移：上/下一个窗口也接受。
	prev, _ := totpCode(secret, at.Add(-30*time.Second))
	if !verifyTOTP(secret, prev, at) {
		t.Fatal("previous window code should verify (clock skew)")
	}
	// 差太远 / 长度不对 / 空 secret：拒绝。
	far, _ := totpCode(secret, at.Add(-5*time.Minute))
	if verifyTOTP(secret, far, at) {
		t.Fatal("code from far window must be rejected")
	}
	if verifyTOTP(secret, "12345", at) {
		t.Fatal("wrong-length code must be rejected")
	}
	if verifyTOTP("", current, at) {
		t.Fatal("empty secret must be rejected")
	}
	if _, err := totpCode("not-base32-!!!", at); err == nil {
		t.Fatal("invalid secret must error")
	}
}
