package scheduler

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"prism-2api/internal/pool"
)

func testPool(t *testing.T, names ...string) *pool.Pool {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		body := `{"accessToken":"` + name + `-tok","refreshToken":"r","api_key":"sk-` + name + `"}`
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := pool.New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		acc := p.Get(name)
		if acc == nil {
			t.Fatalf("missing %s", name)
		}
		acc.ID = name
	}
	return p
}

func TestFillFirstBurnsHighestPriority(t *testing.T) {
	p := testPool(t, "low", "high-a", "high-b")
	p.Get("low").SetPriority(0)
	p.Get("high-a").SetPriority(10)
	p.Get("high-b").SetPriority(10)

	s := New(Config{Strategy: StrategyFillFirst, SessionAffinity: false})
	req := Request{Model: "claude-4"}
	first, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Priority() != 10 {
		t.Fatalf("fill-first must stay in high bucket, got %s prio=%d", first.Name, first.Priority())
	}
	second, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != first.Name {
		t.Fatalf("fill-first should burn the same high account until not ready, got %s then %s", first.Name, second.Name)
	}

	// 冷却机制已停用：用 RPM=1 打满模拟「本账号暂不可选」，验证高优桶内轮换。
	if err := p.SetRPM(first.Name, 1); err != nil {
		t.Fatal(err)
	}
	first.TouchUse()
	third, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if third.Priority() != 10 || third.Name == first.Name {
		t.Fatalf("after not-ready should take the other high account, got %s prio=%d", third.Name, third.Priority())
	}

	if err := p.SetRPM(third.Name, 1); err != nil {
		t.Fatal(err)
	}
	third.TouchUse()
	fourth, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Name != "low" {
		t.Fatalf("both high not ready, want low, got %s", fourth.Name)
	}
}

func TestRoundRobinStaysInHighestPriority(t *testing.T) {
	p := testPool(t, "low", "a", "b")
	p.Get("low").SetPriority(0)
	p.Get("a").SetPriority(5)
	p.Get("b").SetPriority(5)
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: false})
	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		acc, err := s.Pick(p.Accounts(), Request{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		if acc.Priority() != 5 {
			t.Fatalf("rr leaked to low priority: %s", acc.Name)
		}
		seen[acc.Name]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("rr should rotate high bucket: %v", seen)
	}
}

// L1 温热优先：tier 内有 sandbox 温热的账号时只在温组里选；全冷则回落全 tier。
func TestWarmPreferencePick(t *testing.T) {
	p := testPool(t, "cold-a", "cold-b", "warm-c")
	p.Get("warm-c").SetSandboxProbe(func() (bool, int64) { return true, 10 })

	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: false, WarmPreference: true})
	req := Request{Model: "gpt-5.6-sol"}
	for i := range 6 {
		acc, err := s.Pick(p.Accounts(), req)
		if err != nil {
			t.Fatal(err)
		}
		if acc.Name != "warm-c" {
			t.Fatalf("pick %d: warm_preference 应只选温热账号, got %s", i, acc.Name)
		}
	}

	// 温热账号 RPM 打满暂不可选后，应回落到冷账号而不是报无可用（冷却机制已停用）。
	if err := p.SetRPM("warm-c", 1); err != nil {
		t.Fatal(err)
	}
	p.Get("warm-c").TouchUse()
	acc, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Name == "warm-c" {
		t.Fatalf("温热账号不可选后应落冷组, got %s", acc.Name)
	}

	// 开关关闭时不分温冷：RR 应轮到冷账号。
	s2 := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: false, WarmPreference: false})
	seen := map[string]bool{}
	for range 6 {
		acc, err := s2.Pick(p.Accounts(), req)
		if err != nil {
			t.Fatal(err)
		}
		seen[acc.Name] = true
	}
	if !seen["cold-a"] || !seen["cold-b"] {
		t.Fatalf("warm_preference=off 时 RR 应覆盖冷账号, got %v", seen)
	}
}

func TestSessionAffinityOutranksHigherPriority(t *testing.T) {
	p := testPool(t, "bound", "better")
	p.Get("bound").SetPriority(1)
	p.Get("better").SetPriority(9)
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: true, SessionAffinityTTL: time.Hour})
	body := []byte(`{"metadata":{"user_id":"user_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`)
	req := Request{Model: "claude-4", Body: body, Headers: http.Header{}}

	// 冷却机制已停用：用 RPM 打满让高优账号暂不可选，验证仍选 bound。
	if err := p.SetRPM("better", 1); err != nil {
		t.Fatal(err)
	}
	p.Get("better").TouchUse()
	first, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "bound" {
		t.Fatalf("high not ready, want bound, got %s", first.Name)
	}
	p.Get("better").MarkSuccess()
	second, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != "bound" {
		t.Fatalf("established binding must outrank recovered higher priority, got %s", second.Name)
	}
}

func TestTriedSkipsFailedAccount(t *testing.T) {
	p := testPool(t, "a", "b")
	s := New(Config{Strategy: StrategyFillFirst, SessionAffinity: false})
	first, err := s.Pick(p.Accounts(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Pick(p.Accounts(), Request{Model: "m", Tried: map[string]struct{}{first.ID: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Name == first.Name {
		t.Fatal("tried set must skip the failed account")
	}
}

// 冷却机制已停用：全部账号 RPM 打满暂不可选时仍要返回错误（无可用账号）。
func TestAllNotReadyReturnsUnavailable(t *testing.T) {
	p := testPool(t, "a")
	if err := p.SetRPM("a", 1); err != nil {
		t.Fatal(err)
	}
	p.Get("a").TouchUse()
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: false})
	if _, err := s.Pick(p.Accounts(), Request{Model: "m"}); err == nil {
		t.Fatal("all accounts not ready should return error")
	}
}

func TestExtractSessionIDs(t *testing.T) {
	primary, _ := ExtractSessionIDs(http.Header{"X-Session-Id": []string{"abc"}}, nil)
	if primary != "header:abc" {
		t.Fatalf("x-session-id = %q", primary)
	}
	primary, _ = ExtractSessionIDs(nil, []byte(`{"metadata":{"user_id":"user_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`))
	if primary != "claude:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("claude metadata = %q", primary)
	}
	primary, fallback := ExtractSessionIDs(nil, []byte(`{"prompt_cache_key":"cache","conversation":{"id":"c1"}}`))
	if primary != "pck:cache" || fallback != "conv:c1" {
		t.Fatalf("pck/conv = %q / %q", primary, fallback)
	}
}

func TestRPMLimitSkipsAccount(t *testing.T) {
	p := testPool(t, "limited", "other")
	p.Get("limited").SetPriority(10)
	p.Get("other").SetPriority(0)
	if err := p.SetRPM("limited", 1); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Strategy: StrategyFillFirst, SessionAffinity: false})
	first, err := s.Pick(p.Accounts(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "limited" {
		t.Fatalf("want limited, got %s", first.Name)
	}
	first.MarkSuccess()
	second, err := s.Pick(p.Accounts(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != "other" {
		t.Fatalf("rpm=1 should skip limited after one success, got %s", second.Name)
	}
}

func TestGroupOverlayRPMSkipsAccount(t *testing.T) {
	p := testPool(t, "vip", "plain")
	p.Get("vip").SetPriority(10)
	p.Get("plain").SetPriority(0)
	p.Get("vip").SetGroups([]string{"team"})
	s := New(Config{Strategy: StrategyFillFirst, SessionAffinity: false})
	s.SetLimitResolver(func(gs []string) (int, int) {
		for _, g := range gs {
			if g == "team" {
				return 1, 0
			}
		}
		return 0, 0
	})
	first, err := s.Pick(p.Accounts(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "vip" {
		t.Fatalf("want vip, got %s", first.Name)
	}
	first.TouchUse()
	second, err := s.Pick(p.Accounts(), Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != "plain" {
		t.Fatalf("group overlay rpm=1 should skip vip, got %s", second.Name)
	}
}

// 客户端不带任何会话标识时，靠调用方给的兜底亲和键粘住同一账号。
// 动机（真机）：热沙箱缓存只对同一账号有效，不粘的话每轮都可能落到冷账号，
// 于是每轮重付 7~35s 的上游沙箱预热。
func TestFallbackAffinitySticksSameAccount(t *testing.T) {
	p := testPool(t, "a", "b", "c")
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: true, SessionAffinityTTL: time.Hour})
	req := Request{Model: "gpt-5.6-terra", Headers: http.Header{}, FallbackAffinity: "key:42"}

	first, err := s.Pick(p.Accounts(), req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		got, err := s.Pick(p.Accounts(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != first.Name {
			t.Fatalf("同一兜底键应当粘住同一账号：第 %d 次拿到 %s（首次 %s）", i+2, got.Name, first.Name)
		}
	}
}

// 不同调用方（不同 API Key）仍要分散到不同账号，否则一个 key 会把所有流量压在同一个号上。
func TestFallbackAffinitySpreadsAcrossKeys(t *testing.T) {
	p := testPool(t, "a", "b", "c")
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: true, SessionAffinityTTL: time.Hour})

	seen := map[string]bool{}
	for _, key := range []string{"key:1", "key:2", "key:3", "key:4", "key:5", "key:6"} {
		acc, err := s.Pick(p.Accounts(), Request{
			Model: "gpt-5.6-terra", Headers: http.Header{}, FallbackAffinity: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		seen[acc.Name] = true
	}
	if len(seen) < 2 {
		t.Fatalf("不同 key 应当分散到多个账号，实际只用了 %v", seen)
	}
}

// 客户端自带会话标识时，客户端的意图优先于兜底键。
func TestClientSessionOutranksFallbackAffinity(t *testing.T) {
	p := testPool(t, "a", "b")
	s := New(Config{Strategy: StrategyRoundRobin, SessionAffinity: true, SessionAffinityTTL: time.Hour})

	bound, err := s.Pick(p.Accounts(), Request{
		Model: "gpt-5.6-terra", Headers: http.Header{},
		Body: []byte(`{"metadata":{"user_id":"user_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Pick(p.Accounts(), Request{
		Model:            "gpt-5.6-terra",
		Headers:          http.Header{"Session-Id": []string{"sess-real"}},
		Body:             []byte(`{"metadata":{"user_id":"user_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`),
		FallbackAffinity: "key:99",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != bound.Name {
		t.Fatalf("客户端会话标识应当优先：want %s, got %s", bound.Name, got.Name)
	}
}
