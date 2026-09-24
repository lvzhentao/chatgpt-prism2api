package emulation

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

const (
	idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	idSuffixN  = 25 // 与 sub2api / 官方 msg_01 + base62 对齐
)

// MessageID 生成官方形态的消息 ID（msg_01 + 25 位 base62）。
func MessageID() string { return prefixedID("msg_01") }

// RequestID 生成官方形态的 request-id（req_01 + 25 位 base62）。
func RequestID() string { return prefixedID("req_01") }

// ToolID 生成客户端 tool_use ID（toolu_01 + 25 位 base62）。
func ToolID() string { return prefixedID("toolu_01") }

// ServerToolID 生成本地 server_tool_use ID（srvtoolu_01 + 25 位 base62）。
func ServerToolID() string { return prefixedID("srvtoolu_01") }

// ContainerID 生成 code_execution 容器 ID。
func ContainerID() string { return prefixedID("container_01") }

func prefixedID(prefix string) string {
	return prefix + randomID(idSuffixN)
}

func randomID(n int) string {
	if n <= 0 {
		return ""
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	}
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		sb.WriteByte(idAlphabet[int(b[i%len(b)])%len(idAlphabet)])
	}
	return sb.String()
}

// IsServerToolID 判断是否为本网关合成的 server tool 块。
func IsServerToolID(id string) bool {
	return strings.HasPrefix(id, "srvtoolu_")
}
