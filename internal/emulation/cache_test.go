package emulation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCacheHitOnSecondRequest(t *testing.T) {
	st := mustStore(t)
	body := cacheBody("stable", false)
	in := 2000
	p1 := st.PrepareCache(7, body, "claude-sonnet-4-6", in)
	if p1 == nil || p1.Result() == nil {
		t.Fatal("first plan missing")
	}
	if p1.Result().InputTokens == 0 || p1.Result().CacheCreationInputTokens == 0 {
		t.Fatalf("first should write cache and keep uncached tail: %+v", p1.Result())
	}
	assertUsage(t, p1.Result(), in, 0, p1.Result().CacheCreationInputTokens, p1.Result().InputTokens)
	p1.Commit()

	p2 := st.PrepareCache(7, body, "claude-sonnet-4-6", in)
	if p2 == nil || p2.Result() == nil {
		t.Fatal("second plan missing")
	}
	if p2.Result().CacheReadInputTokens == 0 || p2.Result().InputTokens == 0 {
		t.Fatalf("second should hit cache and keep uncached tail: %+v", p2.Result())
	}
	assertUsage(t, p2.Result(), in, p2.Result().CacheReadInputTokens, p2.Result().CacheCreationInputTokens, p2.Result().InputTokens)
}

func TestCachePrepareDoesNotMutateUntilCommit(t *testing.T) {
	st := mustStore(t)
	body := cacheBody("deferred", false)
	a := st.PrepareCache(9, body, "claude-sonnet-4-6", 2000)
	b := st.PrepareCache(9, body, "claude-sonnet-4-6", 2000)
	if a.Result().CacheReadInputTokens != 0 || b.Result().CacheReadInputTokens != 0 {
		t.Fatalf("prepare must not write tracker: a=%+v b=%+v", a.Result(), b.Result())
	}
	b.Commit()
	c := st.PrepareCache(9, body, "claude-sonnet-4-6", 2000)
	if c.Result().CacheReadInputTokens == 0 || c.Result().InputTokens == 0 {
		t.Fatalf("after commit want read+tail got %+v", c.Result())
	}
}

func TestCacheRatioScalesTokens(t *testing.T) {
	st := mustStore(t)
	cfg := st.Config()
	// 模拟旧客户端只提交单值（区间字段为零 → normalize 派生）。
	cfg.Cache.Ratio = 0.5
	cfg.Cache.RatioMin = 0
	cfg.Cache.RatioMax = 0
	if _, err := st.Update(cfg); err != nil {
		t.Fatal(err)
	}
	usage := st.PrepareCache(3, cacheBody("ratio", false), "claude-sonnet-4-6", 2000).Result()
	assertUsage(t, usage, 2000, 0, 1000, 1000)
}

func TestCacheIndependentRatios(t *testing.T) {
	st := mustStore(t)
	cfg := st.Config()
	cfg.Cache.Mode = ModeIndependent
	cfg.Cache.CreationRatio = 0.75
	cfg.Cache.ReadRatio = 0.25
	// 模拟旧客户端只提交单值（区间字段为零 → normalize 派生）。
	cfg.Cache.ReadRatioMin = 0
	cfg.Cache.ReadRatioMax = 0
	if _, err := st.Update(cfg); err != nil {
		t.Fatal(err)
	}
	body := cacheBody("independent", false)
	first := st.PrepareCache(4, body, "claude-sonnet-4-6", 2000)
	assertUsage(t, first.Result(), 2000, 0, 1500, 500)
	first.Commit()
	second := st.PrepareCache(4, body, "claude-sonnet-4-6", 2000)
	assertUsage(t, second.Result(), 2000, 500, 0, 1500)
}

func TestCacheRatioRangeSampling(t *testing.T) {
	st := mustStore(t)
	cfg := st.Config()
	cfg.Cache.RatioMin = 0.9
	cfg.Cache.RatioMax = 0.95
	if _, err := st.Update(cfg); err != nil {
		t.Fatal(err)
	}
	body := cacheBody("range", false)
	st.PrepareCache(11, body, "claude-sonnet-4-6", 2000).Commit()
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		u := st.PrepareCache(11, body, "claude-sonnet-4-6", 2000).Result()
		if u == nil {
			t.Fatal("plan missing")
		}
		// reserveUncachedTail 会偷走约 2 token 记入 input，容差放宽。
		if u.CacheReadInputTokens < 2000*0.9-2 || u.CacheReadInputTokens > 2000*0.95 {
			t.Fatalf("read %d outside [90%%,95%%] of 2000: %+v", u.CacheReadInputTokens, u)
		}
		seen[u.CacheReadInputTokens] = true
	}
	if len(seen) < 2 {
		t.Fatalf("read ratio should jitter in range, got fixed %v", seen)
	}
}

func TestCacheIndependentReadRange(t *testing.T) {
	st := mustStore(t)
	cfg := st.Config()
	cfg.Cache.Mode = ModeIndependent
	cfg.Cache.CreationRatio = 1
	cfg.Cache.ReadRatioMin = 0.9
	cfg.Cache.ReadRatioMax = 0.95
	if _, err := st.Update(cfg); err != nil {
		t.Fatal(err)
	}
	body := cacheBody("ind-range", false)
	first := st.PrepareCache(13, body, "claude-sonnet-4-6", 2000)
	if first.Result().CacheCreationInputTokens != 2000-2 {
		t.Fatalf("first round should write full cache: %+v", first.Result())
	}
	first.Commit()
	u := st.PrepareCache(13, body, "claude-sonnet-4-6", 2000).Result()
	if u.CacheCreationInputTokens != 0 {
		t.Fatalf("second round should be pure read: %+v", u)
	}
	if u.CacheReadInputTokens < 2000*0.9-2 || u.CacheReadInputTokens > 2000*0.95 {
		t.Fatalf("read %d outside [90%%,95%%] of 2000: %+v", u.CacheReadInputTokens, u)
	}
}

func TestCacheForceHit(t *testing.T) {
	st := mustStore(t)
	cfg := st.Config()
	cfg.Cache.RatioMin = 0.9
	cfg.Cache.RatioMax = 0.95
	cfg.Cache.ForceHit = true
	if _, err := st.Update(cfg); err != nil {
		t.Fatal(err)
	}
	// 未 Commit 任何前缀：首轮也应直接报 read，无 creation。
	u := st.PrepareCache(14, cacheBody("force", false), "claude-sonnet-4-6", 2000).Result()
	if u == nil {
		t.Fatal("plan missing")
	}
	if u.CacheCreationInputTokens != 0 {
		t.Fatalf("force hit must not report creation: %+v", u)
	}
	if u.CacheReadInputTokens < 2000*0.9-2 || u.CacheReadInputTokens > 2000*0.95 {
		t.Fatalf("read %d outside [90%%,95%%] of 2000: %+v", u.CacheReadInputTokens, u)
	}
}

func TestNormalizeRatioRange(t *testing.T) {
	if lo, hi := normalizeRatioRange(0, 0, 0.5); lo != 0.5 || hi != 0.5 {
		t.Fatalf("derive from single value: %v %v", lo, hi)
	}
	if lo, hi := normalizeRatioRange(0.95, 0.9, 1); lo != 0.9 || hi != 0.95 {
		t.Fatalf("swap reversed range: %v %v", lo, hi)
	}
	if lo, hi := normalizeRatioRange(1.2, 1.5, 1); lo != 1 || hi != 1 {
		t.Fatalf("clamp overflow: %v %v", lo, hi)
	}
	if lo, hi := normalizeRatioRange(0, 0.5, 1); lo != 0 || hi != 0.5 {
		t.Fatalf("partial range must not derive: %v %v", lo, hi)
	}
	if r := sampleRatioRange(0.5, 0.5); r != 0.5 {
		t.Fatalf("degenerate range should be fixed: %v", r)
	}
	for i := 0; i < 100; i++ {
		if r := sampleRatioRange(0.9, 0.95); r < 0.9 || r > 0.95 {
			t.Fatalf("sample %v outside [0.9,0.95]", r)
		}
	}
}

// P2 大 prompt 指纹截断：总 token 超阈值时只对前缀块做指纹，但 token 记账经
// scaleBreakpointsToInputTokens 按比例放大到全量，缓存命中占比观感不变。
func TestCacheFastPathLargePrompt(t *testing.T) {
	st := mustStore(t)
	body := cacheBody("fastpath", false)
	first := st.PrepareCache(21, body, "claude-sonnet-4-6", 150000)
	if first == nil {
		t.Fatal("fast path should still cache")
	}
	if first.Result().CacheCreationInputTokens < 150000*9/10 {
		t.Fatalf("creation should scale to full input: %+v", first.Result())
	}
	first.Commit()
	second := st.PrepareCache(21, body, "claude-sonnet-4-6", 150000).Result()
	if second == nil || second.CacheReadInputTokens < 150000*9/10 {
		t.Fatalf("read should scale to full input: %+v", second)
	}
}

func TestCacheContentChangeMisses(t *testing.T) {
	st := mustStore(t)
	_ = st.PrepareCache(5, cacheBody("before", false), "claude-sonnet-4-6", 2000)
	st.PrepareCache(5, cacheBody("before", false), "claude-sonnet-4-6", 2000).Commit()
	changed := st.PrepareCache(5, cacheBody("after", false), "claude-sonnet-4-6", 2000)
	if changed.Result().CacheReadInputTokens != 0 || changed.Result().CacheCreationInputTokens == 0 {
		t.Fatalf("content change should miss: %+v", changed.Result())
	}
}

func TestCacheOneHourBucket(t *testing.T) {
	st := mustStore(t)
	usage := st.PrepareCache(6, cacheBody("1h", true), "claude-sonnet-4-6", 2000).Result()
	if usage.CacheCreation1h == 0 || usage.CacheCreation5m != 0 {
		t.Fatalf("1h bucket %+v", usage)
	}
}

func TestCacheFallbackWithoutCacheControl(t *testing.T) {
	st := mustStore(t)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"` + strings.Repeat("fallback chunk ", 400) + `"}]}`)
	first := st.PrepareCache(8, body, "claude-sonnet-4-6", 2000)
	if first == nil {
		t.Fatal("fallback should still cache")
	}
	first.Commit()
	second := st.PrepareCache(8, body, "claude-sonnet-4-6", 2000)
	if second.Result().CacheReadInputTokens == 0 {
		t.Fatalf("fallback second %+v", second.Result())
	}
}

func TestCacheBillingHeaderDoesNotBustPrefix(t *testing.T) {
	st := mustStore(t)
	mk := func(ver string) []byte {
		sys := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s", ver)
		payload := map[string]any{
			"model": "claude-sonnet-4-6",
			"system": []any{
				map[string]any{"type": "text", "text": sys, "cache_control": map[string]any{"type": "ephemeral"}},
			},
			"messages": []any{
				map[string]any{"role": "user", "content": strings.Repeat("cacheable prompt chunk stable ", 512)},
			},
		}
		b, _ := json.Marshal(payload)
		return b
	}
	p1 := st.PrepareCache(11, mk("2.1.153"), "claude-sonnet-4-6", 4000)
	p1.Commit()
	p2 := st.PrepareCache(11, mk("2.1.233"), "claude-sonnet-4-6", 4000)
	if p2.Result().CacheReadInputTokens <= 0 {
		t.Fatalf("billing header should be canonicalized: %+v", p2.Result())
	}
}

func TestCacheHonorsSmallExplicitBreakpointOnOpus(t *testing.T) {
	st := mustStore(t)
	mk := func(round int) []byte {
		msgs := []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "[cachecheck round 0] Do not call any tools."},
			}},
		}
		for i := 1; i <= round; i++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "Cache probe acknowledged; no tools were called."},
				}},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": fmt.Sprintf("[cachecheck round %d] Do not call any tools.", i), "cache_control": map[string]any{"type": "ephemeral"}},
				}},
			)
		}
		if round == 0 {
			msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		payload := map[string]any{
			"model": "claude-opus-4-8",
			"system": []any{
				map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.233"},
				map[string]any{"type": "text", "text": " You are an AI agent for coding.  [cachecheck mode]", "cache_control": map[string]any{"type": "ephemeral"}},
			},
			"tools": []any{
				map[string]any{"name": "Write"},
				map[string]any{"name": "WebSearch"},
			},
			"messages": msgs,
		}
		b, _ := json.Marshal(payload)
		return b
	}
	first := st.PrepareCache(21, mk(0), "claude-opus-4-8", 3800)
	if first == nil || first.Result() == nil || first.Result().CacheCreationInputTokens == 0 {
		t.Fatalf("opus small cache_control should still write: %+v", first)
	}
	first.Commit()
	second := st.PrepareCache(21, mk(1), "claude-opus-4-8", 3850)
	if second == nil || second.Result().CacheReadInputTokens == 0 {
		t.Fatalf("round 1 should hit cached prefix: %+v", second.Result())
	}
}

func TestCachePreviewInvariant(t *testing.T) {
	cfg := DefaultConfig().Cache
	cfg.Ratio = 0.4
	first, second := cfg.Preview(20000)
	if first.total() != 20000 || second.total() != 20000 {
		t.Fatalf("preview invariant first=%+v second=%+v", first, second)
	}
	if first.CacheCreationInputTokens != 8000 || first.InputTokens != 12000 {
		t.Fatalf("first %+v", first)
	}
	if second.CacheReadInputTokens != 8000 || second.InputTokens != 12000 {
		t.Fatalf("second %+v", second)
	}
}

func TestReserveUncachedTailKeepsInvariant(t *testing.T) {
	u := &Usage{CacheCreationInputTokens: 2000, CacheCreation5m: 2000}
	reserveUncachedTail(u, 2000)
	if u.InputTokens != 2 || u.CacheCreationInputTokens != 1998 {
		t.Fatalf("tail %+v", u)
	}
	if u.total() != 2000 {
		t.Fatalf("invariant %+v", u)
	}
	u2 := &Usage{CacheReadInputTokens: 18000}
	reserveUncachedTail(u2, 18000)
	if u2.InputTokens != 2 || u2.CacheReadInputTokens+u2.InputTokens != 18000 {
		t.Fatalf("read tail %+v", u2)
	}
}

func TestClampRatio(t *testing.T) {
	if clampRatio(1.4, 1) != 1 || clampRatio(-0.2, 1) != 0 || clampRatio(0, 1) != 0 {
		t.Fatal("clamp")
	}
}

func mustStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func assertUsage(t *testing.T, u *Usage, total, read, create, input int) {
	t.Helper()
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.total() != total {
		t.Fatalf("invariant %d+%d+%d != %d", u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, total)
	}
	if u.CacheReadInputTokens != read || u.CacheCreationInputTokens != create || u.InputTokens != input {
		t.Fatalf("usage %+v want read=%d create=%d input=%d", u, read, create, input)
	}
}

func cacheBody(label string, oneHour bool) []byte {
	ttl := ""
	if oneHour {
		ttl = `,"ttl":"1h"`
	}
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"%s}}]}]}`, strings.Repeat("cacheable prompt chunk "+label+" ", 512), ttl))
}
