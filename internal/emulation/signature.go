package emulation

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"math/big"
	"sync"
)

const (
	signatureCacheLimit = 1000
	// officialClaudeChannelID 是 opus-4-6/4-7/4-8 官方 Messages API 的 channel_id 16。
	// 旧 Anthropic Max compact 才是 12；HVOY 测的是 opus-4-8，12 会被打成 FORGED。
	officialClaudeChannelID   = 16
	officialSignatureVersion  = 2
	officialThinkingBlockKind = "thinking"
	officialContainerMetaLen  = 80
)

type sigCacheEntry struct {
	key   string
	value string
}

var (
	sigLRU  = list.New()
	sigMap  = make(map[string]*list.Element)
	sigLock sync.Mutex
)

// ThinkingSignature 生成 thinking 块的 signature。
// 外形对齐官方 opus-4-8：单层 E 信封 + channel 16 + model_text + thinking + UUID。
// HVOY 要通过：verdict=PASS，且 field 6 能归一成 opus-4-8。
func ThinkingSignature(content, model, messageID string) string {
	cacheKey := hash16(content + ":" + model + ":" + messageID)
	sigLock.Lock()
	if elem, ok := sigMap[cacheKey]; ok {
		sigLRU.MoveToFront(elem)
		sigLock.Unlock()
		if entry, ok := elem.Value.(*sigCacheEntry); ok && entry != nil {
			return entry.value
		}
		return ""
	}
	sigLock.Unlock()

	sig := generateThinkingSignature(content, model)

	sigLock.Lock()
	for sigLRU.Len() >= signatureCacheLimit {
		if oldest := sigLRU.Back(); oldest != nil {
			if entry, ok := oldest.Value.(*sigCacheEntry); ok && entry != nil {
				delete(sigMap, entry.key)
			}
			sigLRU.Remove(oldest)
		}
	}
	entry := &sigCacheEntry{key: cacheKey, value: sig}
	sigMap[cacheKey] = sigLRU.PushFront(entry)
	sigLock.Unlock()
	return sig
}

func generateThinkingSignature(thinking, model string) string {
	nonce := make([]byte, 12)
	session := make([]byte, 12)
	meta := make([]byte, officialContainerMetaLen)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	if _, err := rand.Read(session); err != nil {
		return ""
	}
	if _, err := rand.Read(meta); err != nil {
		return ""
	}
	ctxID := randomUUID()
	if ctxID == "" {
		return ""
	}

	digest := sha512.New384()
	_, _ = digest.Write(nonce)
	_, _ = digest.Write(session)
	_, _ = digest.Write([]byte(thinking))
	sha384 := digest.Sum(nil)

	ecdsaRaw := p256Signature(sha384)
	if len(ecdsaRaw) != 64 {
		return ""
	}

	channel := appendProtobufVarintField(nil, 1, officialClaudeChannelID)
	channel = appendProtobufVarintField(channel, 3, officialSignatureVersion)
	channel = appendProtobufBytes(channel, 5, ecdsaRaw)
	if model != "" {
		channel = appendProtobufBytes(channel, 6, []byte(model))
	}
	channel = appendProtobufVarintField(channel, 7, 1)
	channel = appendProtobufBytes(channel, 8, []byte(officialThinkingBlockKind))
	channel = appendProtobufBytes(channel, 11, []byte(ctxID))

	inner := appendProtobufBytes(nil, 1, channel)
	inner = appendProtobufBytes(inner, 2, nonce)
	inner = appendProtobufBytes(inner, 3, session)
	inner = appendProtobufBytes(inner, 4, sha384)
	inner = appendProtobufBytes(inner, 5, meta)

	body := appendProtobufBytes(nil, 2, inner)
	body = appendProtobufVarintField(body, 3, 1)
	return base64.StdEncoding.EncodeToString(body)
}

func p256Signature(digest []byte) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(digest)
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil || r == nil || s == nil {
		return nil
	}
	n := elliptic.P256().Params().N
	half := new(big.Int).Rsh(new(big.Int).Set(n), 1)
	if s.Cmp(half) == 1 {
		s.Sub(n, s)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func hash16(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:16])
}

func appendProtobufVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendProtobufBytes(dst []byte, field int, data []byte) []byte {
	dst = append(dst, byte(field<<3)|0x02)
	dst = appendProtobufVarint(dst, uint64(len(data)))
	return append(dst, data...)
}

func appendProtobufVarintField(dst []byte, field int, v uint64) []byte {
	dst = append(dst, byte(field<<3))
	return appendProtobufVarint(dst, v)
}

// SignatureInfo 是检测站会读的签名外壳字段。
type SignatureInfo struct {
	ChannelID  uint64
	ChannelLen int
	Model      string
	BlockKind  string
	ContextID  string
	HasChannel bool
}

// InspectThinkingSignature 解析本地/官方形态 thinking signature 的 channel 与 model。
func InspectThinkingSignature(sig string) SignatureInfo {
	var info SignatureInfo
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) == 0 || raw[0] != 0x12 {
		return info
	}
	container, ok := protobufBytesField(raw, 2)
	if !ok {
		return info
	}
	channel, ok := protobufBytesField(container, 1)
	if !ok {
		return info
	}
	info.ChannelLen = len(channel)
	if id, ok := protobufVarintField(channel, 1); ok {
		info.ChannelID = id
		info.HasChannel = true
	}
	if model, ok := protobufBytesField(channel, 6); ok {
		info.Model = string(model)
	}
	if kind, ok := protobufBytesField(channel, 8); ok {
		info.BlockKind = string(kind)
	}
	if ctx, ok := protobufBytesField(channel, 11); ok {
		info.ContextID = string(ctx)
	}
	return info
}

func protobufBytesField(msg []byte, field int) ([]byte, bool) {
	for off := 0; off < len(msg); {
		num, typ, n := consumeProtobufTag(msg[off:])
		if n <= 0 {
			return nil, false
		}
		off += n
		if typ != 2 {
			skip, ok := skipProtobufValue(msg[off:], typ)
			if !ok {
				return nil, false
			}
			off += skip
			continue
		}
		ln, n := consumeProtobufVarint(msg[off:])
		if n <= 0 || off+n+int(ln) > len(msg) {
			return nil, false
		}
		off += n
		val := msg[off : off+int(ln)]
		off += int(ln)
		if num == field {
			return val, true
		}
	}
	return nil, false
}

func protobufVarintField(msg []byte, field int) (uint64, bool) {
	for off := 0; off < len(msg); {
		num, typ, n := consumeProtobufTag(msg[off:])
		if n <= 0 {
			return 0, false
		}
		off += n
		if typ != 0 {
			skip, ok := skipProtobufValue(msg[off:], typ)
			if !ok {
				return 0, false
			}
			off += skip
			continue
		}
		v, n := consumeProtobufVarint(msg[off:])
		if n <= 0 {
			return 0, false
		}
		off += n
		if num == field {
			return v, true
		}
	}
	return 0, false
}

func consumeProtobufTag(b []byte) (field, typ, n int) {
	v, n := consumeProtobufVarint(b)
	if n <= 0 {
		return 0, 0, 0
	}
	return int(v >> 3), int(v & 7), n
}

func consumeProtobufVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func skipProtobufValue(b []byte, typ int) (int, bool) {
	switch typ {
	case 0:
		_, n := consumeProtobufVarint(b)
		return n, n > 0
	case 1:
		if len(b) < 8 {
			return 0, false
		}
		return 8, true
	case 2:
		ln, n := consumeProtobufVarint(b)
		if n <= 0 || n+int(ln) > len(b) {
			return 0, false
		}
		return n + int(ln), true
	case 5:
		if len(b) < 4 {
			return 0, false
		}
		return 4, true
	}
	return 0, false
}
