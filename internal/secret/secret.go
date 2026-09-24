// Package secret 用 AES-256-GCM 加密落盘字段（账号 access/refresh/api_key）。
// 密钥来自 WEB2API_ENCRYPT_KEY：64 位 hex 当 32 字节；否则 SHA-256(口令)。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"sync"
)

const prefix = "enc:v1:"

var (
	mu  sync.RWMutex
	key []byte
)

// Configure 设置进程级密钥；空字符串关闭加密。
func Configure(raw string) {
	mu.Lock()
	defer mu.Unlock()
	key = deriveKey(raw)
}

// ConfigureFromEnv 读取 WEB2API_ENCRYPT_KEY。
func ConfigureFromEnv() {
	Configure(os.Getenv("WEB2API_ENCRYPT_KEY"))
}

// Enabled 是否已配置密钥。
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(key) == 32
}

func deriveKey(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// Seal 加密明文。未配置密钥时原样返回。
func Seal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, prefix) {
		return plain
	}
	mu.RLock()
	k := key
	mu.RUnlock()
	if len(k) != 32 {
		return plain
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return plain
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plain
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return plain
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(out)
}

// Open 解密。非本格式或未配置密钥时原样返回（兼容旧明文文件）。
func Open(sealed string) string {
	if !strings.HasPrefix(sealed, prefix) {
		return sealed
	}
	mu.RLock()
	k := key
	mu.RUnlock()
	if len(k) != 32 {
		return sealed
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return sealed
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return sealed
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return sealed
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return sealed
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return sealed
	}
	return string(plain)
}
