package api

import (
	"fmt"
	"testing"
	"time"

	"prism-2api/internal/pool"
)

// warm-set 按 MRU 取 K：使用顺序 b, d, a → MRU: a, d, b；c 从未使用排最后。
func TestSelectWarmSetMRU(t *testing.T) {
	s := setupTestServer(t)
	for _, n := range []string{"acc-a", "acc-b", "acc-c", "acc-d"} {
		if err := s.pool.AddAPIKey(n, "sk-"+n); err != nil {
			t.Fatal(err)
		}
	}
	byName := map[string]*pool.Account{}
	for _, a := range s.pool.Accounts() {
		byName[a.Name] = a
	}
	byName["acc-b"].RecordUse()
	byName["acc-d"].RecordUse()
	byName["acc-a"].RecordUse()

	got := selectWarmSet(s.pool.Accounts(), 2, time.Now())
	if len(got) != 2 || got[0].Name != "acc-a" || got[1].Name != "acc-d" {
		names := []string{}
		for _, a := range got {
			names = append(names, a.Name)
		}
		t.Fatalf("MRU 前 2 应为 [acc-a acc-d]，got %v", names)
	}

	// K 超过总数：4 个全回，从未使用的 acc-c 排最后。
	got = selectWarmSet(s.pool.Accounts(), 10, time.Now())
	if len(got) != 4 || got[3].Name != "acc-c" {
		names := []string{}
		for _, a := range got {
			names = append(names, a.Name)
		}
		t.Fatalf("K=10 应全回且 acc-c 垫底，got %v", names)
	}

	// 停用跳过：acc-a 停用后前 2 变为 acc-d, acc-b。
	off := false
	byName["acc-a"].ApplyAdminPatch(pool.AdminPatch{Enabled: &off})
	got = selectWarmSet(s.pool.Accounts(), 2, time.Now())
	if len(got) != 2 || got[0].Name != "acc-d" || got[1].Name != "acc-b" {
		names := []string{}
		for _, a := range got {
			names = append(names, a.Name)
		}
		t.Fatalf("停用 acc-a 后前 2 应为 [acc-d acc-b]，got %v", names)
	}

	// K<=0 → 空。
	if out := selectWarmSet(s.pool.Accounts(), 0, time.Now()); len(out) != 0 {
		t.Fatalf("K=0 应返回空，got %d", len(out))
	}

	// 探针未注入时 Snapshot 默认 false/0。
	snap := byName["acc-b"].Snapshot()
	if snap.SandboxWarm || snap.SandboxAgeSec != 0 {
		t.Fatalf("未注入探针时 sandbox 观测应为 false/0，got %v/%d", snap.SandboxWarm, snap.SandboxAgeSec)
	}
}

// 退避序列 1m/2m/4m/5m封顶。
func TestPrewarmBackoffSequence(t *testing.T) {
	want := map[int]time.Duration{
		1:   time.Minute,
		2:   2 * time.Minute,
		3:   4 * time.Minute,
		4:   5 * time.Minute,
		5:   5 * time.Minute,
		100: 5 * time.Minute,
	}
	for n, w := range want {
		if got := prewarmBackoff(n); got != w {
			t.Fatalf("prewarmBackoff(%d) = %s，want %s", n, got, w)
		}
	}
	if got := prewarmBackoff(0); got != 0 {
		t.Fatalf("prewarmBackoff(0) = %s，want 0", got)
	}
}

// 抖动分散：100 账号偏移落在 [0,30s) 秒级网格内、确定性、不过度扎堆。
func TestPrewarmJitterSpread(t *testing.T) {
	seen := map[int64]int{}
	var first time.Duration
	for i := range 100 {
		name := fmt.Sprintf("acc-%03d", i)
		j := prewarmJitter(name)
		if j < 0 || j >= 30*time.Second {
			t.Fatalf("prewarmJitter(%q) = %s，超出 [0,30s)", name, j)
		}
		if j%time.Second != 0 {
			t.Fatalf("prewarmJitter(%q) = %s，非秒级网格", name, j)
		}
		seen[int64(j/time.Second)]++
		if i == 0 {
			first = j
		}
	}
	if again := prewarmJitter("acc-000"); again != first {
		t.Fatalf("抖动应对同名稳定，got %s then %s", first, again)
	}
	if len(seen) < 10 {
		t.Fatalf("100 账号仅覆盖 %d 个偏移槽，过于扎堆", len(seen))
	}
	var thirds [3]bool
	for sec := range seen {
		thirds[sec/10] = true
	}
	for i, hit := range thirds {
		if !hit {
			t.Fatalf("第 %d 个 10s 区间无账号落入，抖动未铺开", i)
		}
	}
}

// interval=10s 时抖动必须按 interval/4（2.5s）封顶：lastOK 后 2.5s 内 due 不得为真。
// 否则抖动（最大 30s）比 interval 还大，due 恒真，每 prewarmMinGap 就跑一轮，上游负载翻倍。
func TestPrewarmDueJitterCappedAtQuarterInterval(t *testing.T) {
	c := &prewarmController{
		failures:    map[string]int{},
		nextAllowed: map[string]time.Time{},
		lastOK:      map[string]time.Time{},
	}
	interval := 10 * time.Second
	lastOK := time.Now()
	// 找一个原始抖动 > 2.5s 的账号（否则封顶前后无区别，测不到回归）。
	name := ""
	for i := range 200 {
		cand := fmt.Sprintf("acc-%03d", i)
		if prewarmJitter(cand) > interval/4 {
			name = cand
			break
		}
	}
	if name == "" {
		t.Fatal("200 个账号里没找到抖动 >2.5s 的，抖动函数可能已变")
	}
	c.lastOK[name] = lastOK
	if c.due(name, interval, lastOK.Add(2*time.Second)) {
		t.Fatalf("interval=10s 时 lastOK 后 2s 不应 due（抖动应封顶 2.5s），账号 %s 原始抖动 %s", name, prewarmJitter(name))
	}
	if !c.due(name, interval, lastOK.Add(interval)) {
		t.Fatalf("interval 到期后应 due，账号 %s", name)
	}
}
