package auth

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefreshConcurrentMergesSingleNetwork：10 路同时 Refresh 只出 1 次网络，
// 合并等待的 9 路拿到与 leader 相同的 token（T2.2 auth 部分验收）。
func TestRefreshConcurrentMergesSingleNetwork(t *testing.T) {
	var calls atomic.Int32
	var callbacks atomic.Int32
	prev := ExchangeHook
	ExchangeHook = func(apiBaseURL, secret, clientVersion, clientType string, client *http.Client) (*Token, error) {
		calls.Add(1)
		// 制造重叠窗口：确保 10 路都在同一 flight 内到达。
		time.Sleep(150 * time.Millisecond)
		return &Token{AccessToken: "merged-access", RefreshToken: "rt-1"}, nil
	}
	t.Cleanup(func() { ExchangeHook = prev })

	tm := NewTokenManager("http://127.0.0.1", "", "test-ver", "ide")
	tm.SetToken(&Token{AccessToken: "old-access", RefreshToken: "rt-1"})
	tm.SetOnRefresh(func(*Token) { callbacks.Add(1) })

	const n = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	toks := make([]*Token, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := tm.Refresh()
			toks[i], errs[i] = tok, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: Refresh err = %v", i, errs[i])
		}
		if toks[i] == nil || toks[i].AccessToken != "merged-access" {
			t.Fatalf("goroutine %d: tok = %+v, want AccessToken %q", i, toks[i], "merged-access")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ExchangeHook calls = %d, want 1", got)
	}
	// store+onRefresh 只由 leader 执行一次。
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("onRefresh calls = %d, want 1", got)
	}
	if cur := tm.Current(); cur == nil || cur.AccessToken != "merged-access" {
		t.Fatalf("Current() = %+v, want merged-access", cur)
	}
}
