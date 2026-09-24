package api

import (
	"context"
	"net/http"
	"strings"
)

type identityKey struct{}

// Identity 是公开 API 鉴权成功后的请求身份（对齐 kiro AuthIdentity：key id + group）。
type Identity struct {
	KeyID uint64
	Group string
}

// WithIdentity 把客户端 Key 身份写入请求上下文。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom 读取请求身份；未鉴权（本地空 api_key_auth）时 ok=false。
func IdentityFrom(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// extractAPIKey 对齐 kiro common/auth.rs：优先 x-api-key，其次 Authorization: Bearer。
func extractAPIKey(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		return k
	}
	authz := r.Header.Get("Authorization")
	if key, ok := strings.CutPrefix(authz, "Bearer "); ok {
		return strings.TrimSpace(key)
	}
	return ""
}
