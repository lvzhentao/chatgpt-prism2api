// Package clientkeys 是对外入口 Key 层（对齐 kiro.rs ClientKeyManager）。
//
// 与上游 Cursor 账号池相互独立：
//   - 上游账号：服务对接 Cursor 的“出口”
//   - 客户端 Key：中转站对外的“入口”（csk_*）
package clientkeys

import (
	"crypto/rand"
	"crypto/subtle"
)

// Prefix 客户端 Key 前缀（对齐 kiro CLIENT_KEY_PREFIX）。
const Prefix = "csk_"

// FileName 旧版 JSON 文件名（仅一次性迁入）。
const FileName = "client_api_keys.json"

// Key 单条客户端 Key。JSON snake_case（本仓库惯例）；语义对齐 kiro ClientKey。
type Key struct {
	ID                       uint64  `json:"id"`
	Key                      string  `json:"key"`
	Name                     string  `json:"name"`
	Description              string  `json:"description,omitempty"`
	Disabled                 bool    `json:"disabled"`
	CreatedAt                string  `json:"created_at"`
	LastUsedAt               string  `json:"last_used_at,omitempty"`
	TotalCalls               uint64  `json:"total_calls"`
	TotalInputTokens         uint64  `json:"total_input_tokens"`
	TotalOutputTokens        uint64  `json:"total_output_tokens"`
	TotalCacheCreationTokens uint64  `json:"total_cache_creation_tokens"`
	TotalCacheReadTokens     uint64  `json:"total_cache_read_tokens"`
	TotalCredits             float64 `json:"total_credits"`
	// Group 绑定的账号分组名；空 = 不绑定，可使用全部账号（与 master apiKey 一致）。
	Group    string `json:"group,omitempty"`
	IsSystem bool   `json:"is_system,omitempty"`
}

// Equal 常量时间比较（subtle.ConstantTimeCompare）。长度不等时立即 false。
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Generate 生成 csk_ + 32 位 base62 随机串（对齐 kiro generate_client_key）。
func Generate() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	out := make([]byte, 32)
	for i, v := range raw {
		out[i] = charset[int(v)%len(charset)]
	}
	return Prefix + string(out)
}

// Mask 脱敏：长 Key 保留前 8 + 后 4（对齐 kiro）；短 Key 也打码，避免系统 Key 整段泄漏。
func Mask(key string) string {
	n := len(key)
	switch {
	case n == 0:
		return ""
	case n <= 8:
		return key[:1] + "..." + key[n-1:]
	case n <= 12:
		return key[:4] + "..." + key[n-4:]
	default:
		return key[:8] + "..." + key[n-4:]
	}
}
