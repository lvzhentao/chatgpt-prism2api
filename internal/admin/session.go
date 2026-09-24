package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SessionManager 管理端登录会话：HMAC 签名的无状态 token（服务端随机密钥）。
// token 格式: base64url(username.exp) "." base64url(hmac_sha256(secret, payload))
type SessionManager struct {
	secret [32]byte
	ttl    time.Duration
}

// NewSessionManager 创建会话管理器（密钥每次启动随机生成，重启后旧会话失效）。
func NewSessionManager() *SessionManager {
	sm := &SessionManager{ttl: 12 * time.Hour}
	if _, err := rand.Read(sm.secret[:]); err != nil {
		panic(err)
	}
	return sm
}

// CookieName 会话 Cookie 名称。
func (sm *SessionManager) CookieName() string { return "c2a_session" }

// Issue 为指定用户签发会话 token。
func (sm *SessionManager) Issue(username string) string {
	payload := username + "." + strconv.FormatInt(time.Now().Add(sm.ttl).Unix(), 10)
	mac := sm.sign(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac)
}

// Verify 校验会话 token，返回用户名与是否有效。
func (sm *SessionManager) Verify(token string) (string, bool) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return "", false
	}
	expected := sm.sign(string(payload))
	if subtle.ConstantTimeCompare(mac, expected) != 1 {
		return "", false
	}
	parts := strings.SplitN(string(payload), ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp < time.Now().Unix() {
		return "", false
	}
	return parts[0], true
}

func (sm *SessionManager) sign(payload string) []byte {
	m := hmac.New(sha256.New, sm.secret[:])
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// SessionCookie 登录成功后的会话 Cookie。
func (sm *SessionManager) SessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     sm.CookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(sm.ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie 登出时的清除 Cookie。
func (sm *SessionManager) ClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sm.CookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
