// Package egress 把账号相关上游请求送到出口：直连、HTTP 正代，或 Resin 反向代理。
// Resin 域名前是 Nginx，CONNECT / SOCKS5 穿不过，必须改写 URL。
package egress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Kind 出口类型。
const (
	KindDirect = "direct"
	KindHTTP   = "http"
	KindResin  = "resin"
)

// Settings 一次请求的出口设定。
type Settings struct {
	Kind          string
	HTTPProxyURL  string
	ResinURL      string
	ResinPlatform string
	Account       string
}

// Platform 默认平台名（Resin Default）。
const DefaultPlatform = "Default"

// RewriteURL 把目标 URL 改写成 Resin 反向代理路径：
//
//	<resin_url>/<Platform>/<protocol>/<host>[:port]/<path>?<query>
func RewriteURL(resinURL, platform string, target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, fmt.Errorf("empty target url")
	}
	base := strings.TrimRight(strings.TrimSpace(resinURL), "/")
	if base == "" {
		return nil, fmt.Errorf("resin_url is empty")
	}
	if platform == "" {
		platform = DefaultPlatform
	}
	proto := strings.ToLower(target.Scheme)
	if proto == "ws" {
		proto = "http"
	}
	if proto == "wss" {
		proto = "https"
	}
	if proto != "http" && proto != "https" {
		return nil, fmt.Errorf("unsupported protocol %q", target.Scheme)
	}
	host := target.Host
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	raw := base + "/" + platform + "/" + proto + "/" + host + path
	if target.RawQuery != "" {
		raw += "?" + target.RawQuery
	}
	return url.Parse(raw)
}

// Transport 是 Resin 反向代理 RoundTripper：改写 URL 并写入 X-Resin-Account。
type Transport struct {
	Base     http.RoundTripper
	ResinURL string
	Platform string
	Account  string
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.URL == nil {
		return nil, fmt.Errorf("resin: empty request url")
	}
	rewritten, err := RewriteURL(t.ResinURL, t.Platform, req.URL)
	if err != nil {
		return nil, fmt.Errorf("resin rewrite: %w", err)
	}
	cloned := req.Clone(req.Context())
	cloned.URL = rewritten
	cloned.Host = rewritten.Host
	cloned.RequestURI = ""
	if t.Account != "" {
		cloned.Header.Set("X-Resin-Account", t.Account)
	}
	resp, err := base.RoundTrip(cloned)
	if err != nil {
		return nil, err
	}
	if rerr := DetectResinError(resp); rerr != nil {
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return nil, rerr
	}
	return resp, nil
}

// ResinError 是 Resin 控制面/网关错误，不应当业务失败重试。
type ResinError struct {
	Status  int
	Code    string
	Message string
}

func (e *ResinError) Error() string {
	if e == nil {
		return "resin error"
	}
	if e.Code != "" {
		return fmt.Sprintf("resin: %s (%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("resin: %s", e.Message)
}

// DetectResinError 识别 {"error":{"code":"...","message":"..."}}。
func DetectResinError(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "json") && resp.StatusCode < 400 {
		return nil
	}
	if resp.StatusCode != 400 && resp.StatusCode != 401 && resp.StatusCode != 404 && resp.StatusCode != 409 && resp.StatusCode != 500 {
		if resp.StatusCode < 400 {
			return nil
		}
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(buf))
	if err != nil || len(bytes.TrimSpace(buf)) == 0 {
		return nil
	}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(buf, &envelope) != nil || envelope.Error == nil || envelope.Error.Code == "" {
		return nil
	}
	switch envelope.Error.Code {
	case "UNAUTHORIZED", "INVALID_ARGUMENT", "NOT_FOUND", "CONFLICT", "INTERNAL":
		return &ResinError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	default:
		return nil
	}
}

// InheritLease 把临时身份的出口 IP 租约继承到稳定账号标识。
func InheritLease(resinURL, platform, parent, next string) error {
	base := strings.TrimRight(strings.TrimSpace(resinURL), "/")
	if base == "" || parent == "" || next == "" || parent == next {
		return nil
	}
	if platform == "" {
		platform = DefaultPlatform
	}
	body, _ := json.Marshal(map[string]string{
		"parent_account": parent,
		"new_account":    next,
	})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/"+platform+"/actions/inherit-lease", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		if rerr := DetectResinError(resp); rerr != nil {
			return rerr
		}
		return fmt.Errorf("inherit-lease: status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return nil
}

// pooledTransports 按出口标识缓存底层 *http.Transport，进程生命周期共享。
// Resin 的按账号头留在外层 Transport{Account}，不做进共享 key。
var pooledTransports sync.Map // string -> *http.Transport

func newPooledBase() *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   30,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func pooledTransport(key string, build func() *http.Transport) *http.Transport {
	if v, ok := pooledTransports.Load(key); ok {
		return v.(*http.Transport)
	}
	actual, _ := pooledTransports.LoadOrStore(key, build())
	return actual.(*http.Transport)
}

func normalizeResinKey(resinURL, platform string) (string, string) {
	base := strings.TrimRight(strings.TrimSpace(resinURL), "/")
	if strings.TrimSpace(platform) == "" {
		platform = DefaultPlatform
	}
	return base, platform
}

// HTTPClient 按 Settings 构造客户端。Resin 模式禁止走环境 HTTP_PROXY。
// 同一种出口共享底层 *http.Transport；返回的 *http.Client 每次新建，Timeout 按调用方传入。
func HTTPClient(s Settings, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	switch strings.ToLower(strings.TrimSpace(s.Kind)) {
	case "", KindDirect:
		tr := pooledTransport("direct", func() *http.Transport {
			t := newPooledBase()
			t.Proxy = http.ProxyFromEnvironment
			return t
		})
		return &http.Client{Timeout: timeout, Transport: tr}, nil
	case KindHTTP:
		if strings.TrimSpace(s.HTTPProxyURL) == "" {
			tr := pooledTransport("direct", func() *http.Transport {
				t := newPooledBase()
				t.Proxy = http.ProxyFromEnvironment
				return t
			})
			return &http.Client{Timeout: timeout, Transport: tr}, nil
		}
		u, err := url.Parse(s.HTTPProxyURL)
		if err != nil {
			return nil, err
		}
		key := "http|" + strings.TrimSpace(s.HTTPProxyURL)
		tr := pooledTransport(key, func() *http.Transport {
			t := newPooledBase()
			t.Proxy = http.ProxyURL(u)
			return t
		})
		return &http.Client{Timeout: timeout, Transport: tr}, nil
	case KindResin:
		base, platform := normalizeResinKey(s.ResinURL, s.ResinPlatform)
		key := "resin|" + base + "|" + platform
		tr := pooledTransport(key, func() *http.Transport {
			t := newPooledBase()
			t.Proxy = nil
			return t
		})
		rt := &Transport{
			Base:     tr,
			ResinURL: s.ResinURL,
			Platform: platform,
			Account:  s.Account,
		}
		return &http.Client{Timeout: timeout, Transport: rt}, nil
	default:
		return nil, fmt.Errorf("unknown egress kind %q", s.Kind)
	}
}

// ProbeEgressIP 经当前出口访问 api.ipify.org，返回出口 IP。
func ProbeEgressIP(s Settings) (string, error) {
	client, err := HTTPClient(s, 15*time.Second)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("probe: status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	ip := strings.TrimSpace(string(raw))
	if ip == "" {
		return "", fmt.Errorf("probe: empty ip")
	}
	return ip, nil
}

// TempIdentity 登录前尚无稳定账号 ID 时的临时粘性标识。
func TempIdentity(prefix string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "tmp"
	}
	return fmt.Sprintf("%s-%d", p, time.Now().UnixNano())
}
