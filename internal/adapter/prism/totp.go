package prism

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTP（RFC 6238）——OpenAI MFA 用的是 SHA1 / 30s / 6 位，与 Google Authenticator 一致。
// 只依赖标准库；参考实现见 gpt注册机/freeagent-producer 的 core/totp.py（同样的算法）。

const (
	totpPeriod = 30
	totpDigits = 6
)

// totpCode 计算 secret 在 at 时刻的 6 位验证码。
// secret 为 Base32（大小写不敏感、允许空格与缺省 padding）。
func totpCode(secret string, at time.Time) (string, error) {
	key, err := decodeBase32Secret(secret)
	if err != nil {
		return "", err
	}
	if len(key) == 0 {
		return "", fmt.Errorf("prism: empty totp secret")
	}
	counter := uint64(at.Unix() / totpPeriod)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, value%mod), nil
}

// verifyTOTP 允许前后各一个时间窗（时钟漂移 / 提交延迟）。
func verifyTOTP(secret, code string, at time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	for _, skew := range []time.Duration{-totpPeriod * time.Second, 0, totpPeriod * time.Second} {
		want, err := totpCode(secret, at.Add(skew))
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// decodeBase32Secret 兼容用户直接粘贴带空格/小写/无 padding 的 secret。
func decodeBase32Secret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.Join(strings.Fields(secret), ""))
	cleaned = strings.TrimRight(cleaned, "=")
	if cleaned == "" {
		return nil, fmt.Errorf("prism: empty totp secret")
	}
	if pad := len(cleaned) % 8; pad != 0 {
		cleaned += strings.Repeat("=", 8-pad)
	}
	key, err := base32.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("prism: invalid totp secret: %w", err)
	}
	return key, nil
}
