package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func jwtWithExp(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func refreshServer(t *testing.T, hits *atomic.Int32, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/exchange_user_api_key" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Token{
			AccessToken:  jwtWithExp(t, time.Now().Add(time.Hour)),
			RefreshToken: "new-refresh",
		})
	}))
	t.Cleanup(srv.Close)

	// ExchangeHook 默认是「未绑定」占位实现；这里绑定一个最小换票实现，
	// 让用例测的是保活语义而不是 adapter 是否绑定。
	prev := ExchangeHook
	ExchangeHook = func(apiBaseURL, secret, clientVersion, clientType string, client *http.Client) (*Token, error) {
		if client == nil {
			client = &http.Client{Timeout: 5 * time.Second}
		}
		body := strings.NewReader(`{"api_key":` + strconv.Quote(secret) + `}`)
		req, err := http.NewRequest(http.MethodPost,
			strings.TrimRight(apiBaseURL, "/")+"/auth/exchange_user_api_key", body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, ErrInvalidAPIKey
		}
		var tok Token
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return nil, err
		}
		return &tok, nil
	}
	t.Cleanup(func() { ExchangeHook = prev })

	return srv
}

func TestKeepaliveRefreshesTokenExpiringIn10Min(t *testing.T) {
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusOK)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(10*time.Minute)),
		RefreshToken: "rt-1",
	})

	ka := NewKeepalive(KeepaliveConfig{})
	ka.Tick([]Ref{{Name: "oauth-a", Tokens: tm}})

	if hits.Load() != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", hits.Load())
	}
	got := tm.Current()
	if got == nil || got.RefreshToken != "new-refresh" {
		t.Fatalf("token not updated: %+v", got)
	}
	if ka.FailureCount("oauth-a") != 0 {
		t.Fatalf("success should clear failures")
	}
}

func TestKeepaliveSkipsFarExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusOK)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(20*time.Minute)),
		RefreshToken: "rt-1",
	})

	ka := NewKeepalive(KeepaliveConfig{})
	ka.Tick([]Ref{{Name: "far", Tokens: tm}})
	if hits.Load() != 0 {
		t.Fatalf("20min remaining should not refresh, hits=%d", hits.Load())
	}
}

func TestKeepaliveSkipsAPIKeyOnlyWithoutRefreshToken(t *testing.T) {
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusOK)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.URL, "sk-user-key", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken: jwtWithExp(t, time.Now().Add(10*time.Minute)),
	})

	ka := NewKeepalive(KeepaliveConfig{})
	ka.Tick([]Ref{{Name: "apikey", Tokens: tm}})
	if hits.Load() != 0 {
		t.Fatalf("api-key-only without refresh token must skip, hits=%d", hits.Load())
	}
}

func TestKeepaliveFailureCountedNotWiped(t *testing.T) {
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusUnauthorized)
	t.Cleanup(srv.Close)

	old := jwtWithExp(t, time.Now().Add(8*time.Minute))
	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{AccessToken: old, RefreshToken: "rt-1"})

	ka := NewKeepalive(KeepaliveConfig{})
	ka.Tick([]Ref{{Name: "bad", Tokens: tm}})
	if hits.Load() != 1 {
		t.Fatalf("expected refresh attempt, hits=%d", hits.Load())
	}
	if ka.FailureCount("bad") != 1 {
		t.Fatalf("failure should be counted, got %d", ka.FailureCount("bad"))
	}
	got := tm.Current()
	if got == nil || got.AccessToken != old {
		t.Fatalf("failed refresh must leave existing token, got %+v", got)
	}
}

func TestKeepaliveEmptyTick(t *testing.T) {
	ka := NewKeepalive(KeepaliveConfig{})
	ka.Tick(nil)
	ka.Tick([]Ref{})
}

func TestTokenDoesNotRefreshAt10Min(t *testing.T) {
	// Token() 宽限 5 分钟；10 分钟剩余不应走网络。保活才应预刷新。
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusOK)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(10*time.Minute)),
		RefreshToken: "rt-1",
	})
	if _, err := tm.Token(); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("Token() should not refresh at 10min remaining, hits=%d", hits.Load())
	}
}

func TestBackoffForExponential(t *testing.T) {
	// 1败1min，逐次翻倍，8败以上封顶1h
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour}
	for c, w := range want {
		if got := backoffFor(c + 1); got != w {
			t.Fatalf("backoffFor(%d) = %v, want %v", c+1, got, w)
		}
	}
}

func TestKeepaliveBackoffSkipsRetryWithinWindow(t *testing.T) {
	// 失败一次后进入退避窗口：同轮再 Tick 不应再发起刷新（hits 不增）。
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusInternalServerError)
	t.Cleanup(srv.Close)

	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(time.Minute)),
		RefreshToken: "rt-1",
	})
	ka := NewKeepalive(KeepaliveConfig{})
	ref := Ref{Name: "acc-backoff", Tokens: tm}
	ka.Tick([]Ref{ref})
	if ka.FailureCount("acc-backoff") != 1 {
		t.Fatalf("first tick should record 1 failure, got %d", ka.FailureCount("acc-backoff"))
	}
	ka.Tick([]Ref{ref}) // 退避窗口内：跳过
	if ka.FailureCount("acc-backoff") != 1 {
		t.Fatalf("backoff window should skip refresh, failures grew to %d", ka.FailureCount("acc-backoff"))
	}
	if hits.Load() < 1 {
		t.Fatalf("expected at least 1 refresh attempt, got %d", hits.Load())
	}
}

func TestTokenLocalNeverRefreshes(t *testing.T) {
	// TokenLocal 是选号热路径的纯本地判断：token 过期时返回 nil 且绝不发起网络。
	var hits atomic.Int32
	srv := refreshServer(t, &hits, http.StatusOK)
	t.Cleanup(srv.Close)

	valid := NewTokenManager(srv.URL, "", "test-ver", "ide")
	valid.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(time.Hour)),
		RefreshToken: "rt-1",
	})
	if tok := valid.TokenLocal(); tok == nil {
		t.Fatal("valid token should be locally ready")
	}
	if hits.Load() != 0 {
		t.Fatalf("TokenLocal must not hit network, hits=%d", hits.Load())
	}

	expired := NewTokenManager(srv.URL, "", "test-ver", "ide")
	expired.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(-time.Minute)),
		RefreshToken: "rt-1",
	})
	if tok := expired.TokenLocal(); tok != nil {
		t.Fatal("expired token must not be locally ready")
	}
	if expired.Current() == nil {
		t.Fatal("Current() still returns the stale token (no side effect)")
	}
	if hits.Load() != 0 {
		t.Fatalf("TokenLocal must not hit network, hits=%d", hits.Load())
	}
}

func TestKeepaliveDeadAccountRemoved(t *testing.T) {
	// 连败达阈值 → OnDead 回调（上层删号），计数清零。
	var reqHits atomic.Int32
	var deadHits atomic.Int32
	var deadName string
	srv := refreshServer(t, &reqHits, http.StatusInternalServerError)
	t.Cleanup(srv.Close)
	tm := NewTokenManager(srv.URL, "", "test-ver", "ide")
	tm.SetToken(&Token{
		AccessToken:  jwtWithExp(t, time.Now().Add(time.Minute)),
		RefreshToken: "rt-1",
	})
	ka := NewKeepalive(KeepaliveConfig{MaxFailures: 3, OnDead: func(name string, _ error) {
		deadHits.Add(1)
		deadName = name
	}})
	for range 3 {
		ka.Tick([]Ref{{Name: "acc-dead", Tokens: tm}})
		// 退避窗口内直接把 nextTry 清掉，模拟到点重试
		ka.mu.Lock()
		delete(ka.nextTry, "acc-dead")
		ka.mu.Unlock()
	}
	if deadHits.Load() != 1 {
		t.Fatalf("OnDead should fire once at threshold, got %d", deadHits.Load())
	}
	if deadName != "acc-dead" {
		t.Fatalf("deadName=%q", deadName)
	}
	if ka.FailureCount("acc-dead") != 0 {
		t.Fatal("failure count must reset after dead disposal")
	}
}
