package prism

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/auth"
)

// ============================================================
// 定制点 3 — 登录 / 续期 / 探活。
// 站点只支持 OpenAI OAuth 弹窗（keycloak + popup-callback），
// 所以不做浏览器登录：账号在管理台按 cookie 导入。
// 凭据 = prism_oai_access_token（10 天）；session cookie 由客户端自举。
// ============================================================

// LoginURL 无浏览器登录，返回空串（管理台只走 cookie 导入）。
func (a *Adapter) LoginURL(websiteURL, challenge, uuid string) string {
	return ""
}

// PollLogin 无浏览器登录流程。
func (a *Adapter) PollLogin(apiBaseURL, uuid, verifier, clientVersion, clientType string, client *http.Client) (*auth.Token, error) {
	return nil, adapter.ErrNotImplemented
}

// ExchangeCredential 校验导入的凭据：用 prism_oai_access_token 换一次 session。
// 也用于内核刷新时的再次校验（凭据失效则返回错误，由 failclass 处理）。
//
// 若 secret 是「重登凭据束」（含 email/password/TOTP），则直接走侧车重新登录——
// 于是内核 keepalive 到点刷新 = 自动重登，账号寿命不再受 10 天 token 限制。
func (a *Adapter) ExchangeCredential(apiBaseURL, secret, clientVersion, clientType string, client *http.Client) (*auth.Token, error) {
	if bundle := ParseCredentialBundle(secret); bundle.CanRelogin() {
		if a.mock {
			return &auth.Token{AccessToken: bundle.OAI, RefreshToken: secret, APIKey: bundle.OAI}, nil
		}
		// 请求路径的 Token() 也会同步走到这里。ctx 不能无限：正常侧车登录
		// ~20-60s，卡死场景（上游 auth 故障/风控页变化）若不限时，选号路径
		// 会被单个 240s 的浏览器流程连环卡住（线上实测 pick 卡 23 分钟）。
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		res, err := ReloginWithBundle(ctx, bundle, nil)
		if err != nil {
			return nil, fmt.Errorf("prism: relogin: %w", err)
		}
		bundle.OAI = res.OAI
		bundle.Refresh = res.Refresh
		if res.StorageState != "" {
			bundle.StatePath = res.StorageState
		}
		updated := bundle.Marshal()
		return &auth.Token{AccessToken: res.OAI, RefreshToken: updated, APIKey: res.OAI}, nil
	}

	cred := parseCredential(secret)
	if cred.OAI == "" {
		return nil, fmt.Errorf("prism: credential must be prism_oai_access_token")
	}
	if a.mock {
		return &auth.Token{AccessToken: cred.OAI, APIKey: cred.OAI}, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	if base == "" {
		base = strings.TrimRight(a.apiBase, "/")
	}
	if base == "" {
		base = "https://prism.openai.com"
	}
	req, err := http.NewRequest(http.MethodPost, base+PathSession, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	req.Header.Set("Cookie", "prism_oai_access_token="+cred.OAI)
	// 上游偶发 503（抓包里出现过 "upstream connect error"），重试几次再判失败。
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 700 * time.Millisecond)
		}
		resp, err := client.Do(cloneRequest(req))
		if err != nil {
			lastErr = fmt.Errorf("prism: verify credential: %w", err)
			continue
		}
		status := resp.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			resp.Body.Close()
			return nil, auth.ErrInvalidAPIKey
		}
		if status == http.StatusTooManyRequests || status >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("prism: verify credential: status %d", status)
			continue
		}
		if status >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("prism: verify credential: status %d", status)
		}
		token := &auth.Token{AccessToken: cred.OAI, APIKey: cred.OAI}
		for _, ck := range resp.Cookies() {
			if ck.Name == "prism_session_token" && ck.Value != "" {
				token.RefreshToken = ck.Value
			}
		}
		resp.Body.Close()
		return token, nil
	}
	return nil, lastErr
}

// cloneRequest 复制一个可重发的请求（原请求体为 nil，只需头与 URL）。
func cloneRequest(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			clone.Body = body
		}
	}
	return clone
}
