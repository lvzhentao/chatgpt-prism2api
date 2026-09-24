package egress

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRewriteURL(t *testing.T) {
	src, _ := url.Parse("https://api.example.com/v1/orders?page=2")
	got, err := RewriteURL("https://resin.example.com/token", "Default", src)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://resin.example.com/token/Default/https/api.example.com/v1/orders?page=2"
	if got.String() != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestRewriteWS(t *testing.T) {
	src, _ := url.Parse("wss://ws.example.com/chat")
	got, err := RewriteURL("https://resin.example.com/t", "Default", src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Path, "/Default/https/ws.example.com/chat") {
		t.Fatalf("path: %s", got.Path)
	}
}

func TestTransportRewritesAndAccountHeader(t *testing.T) {
	var seenHost, seenPath, seenAccount string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		seenPath = r.URL.Path
		seenAccount = r.Header.Get("X-Resin-Account")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()

	tr := &Transport{
		Base:     http.DefaultTransport,
		ResinURL: up.URL,
		Platform: "Default",
		Account:  "acct-1",
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if seenAccount != "acct-1" {
		t.Fatalf("account header: %q", seenAccount)
	}
	if !strings.Contains(seenPath, "/Default/https/api.ipify.org") {
		t.Fatalf("rewritten path: host=%s path=%s", seenHost, seenPath)
	}
}

func TestDetectResinError(t *testing.T) {
	resp := &http.Response{
		StatusCode: 401,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"UNAUTHORIZED","message":"bad token"}}`)),
	}
	err := DetectResinError(resp)
	re, ok := err.(*ResinError)
	if !ok {
		t.Fatalf("want ResinError, got %v", err)
	}
	if re.Code != "UNAUTHORIZED" {
		t.Fatalf("code: %s", re.Code)
	}
}

func TestHTTPClientSharesBaseTransport(t *testing.T) {
	a, err := HTTPClient(Settings{Kind: KindHTTP, HTTPProxyURL: "http://127.0.0.1:18081"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HTTPClient(Settings{Kind: KindHTTP, HTTPProxyURL: "http://127.0.0.1:18081"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ta, ok := a.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("want *http.Transport, got %T", a.Transport)
	}
	tb, ok := b.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("want *http.Transport, got %T", b.Transport)
	}
	if ta != tb {
		t.Fatalf("same proxy should share underlying *http.Transport")
	}
	if ta.MaxIdleConns != 100 || ta.MaxIdleConnsPerHost != 30 {
		t.Fatalf("pool params: maxIdle=%d perHost=%d", ta.MaxIdleConns, ta.MaxIdleConnsPerHost)
	}
	if ta.IdleConnTimeout != 90*time.Second || ta.TLSHandshakeTimeout != 10*time.Second || ta.ResponseHeaderTimeout != 30*time.Second || ta.ExpectContinueTimeout != time.Second {
		t.Fatalf("timeout params not set: idle=%v tls=%v header=%v expect=%v", ta.IdleConnTimeout, ta.TLSHandshakeTimeout, ta.ResponseHeaderTimeout, ta.ExpectContinueTimeout)
	}
}

func TestHTTPClientProxyIsolation(t *testing.T) {
	a, err := HTTPClient(Settings{Kind: KindHTTP, HTTPProxyURL: "http://127.0.0.1:18082"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HTTPClient(Settings{Kind: KindHTTP, HTTPProxyURL: "http://127.0.0.1:18083"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Transport == b.Transport {
		t.Fatalf("different proxy addresses must not share *http.Transport")
	}
}

func TestHTTPClientResinSharesBaseAcrossAccounts(t *testing.T) {
	a, err := HTTPClient(Settings{Kind: KindResin, ResinURL: "https://resin.example.com/t", ResinPlatform: "Default", Account: "acct-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HTTPClient(Settings{Kind: KindResin, ResinURL: "https://resin.example.com/t", ResinPlatform: "Default", Account: "acct-2"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ra, ok := a.Transport.(*Transport)
	if !ok {
		t.Fatalf("want *Transport, got %T", a.Transport)
	}
	rb, ok := b.Transport.(*Transport)
	if !ok {
		t.Fatalf("want *Transport, got %T", b.Transport)
	}
	if ra.Account != "acct-1" || rb.Account != "acct-2" {
		t.Fatalf("per-request account header must be kept outside shared transport: %q %q", ra.Account, rb.Account)
	}
	ba, ok := ra.Base.(*http.Transport)
	if !ok {
		t.Fatalf("want shared *http.Transport base, got %T", ra.Base)
	}
	if bb, ok := rb.Base.(*http.Transport); !ok || bb != ba {
		t.Fatalf("same resin url+platform should share underlying *http.Transport")
	}
}
