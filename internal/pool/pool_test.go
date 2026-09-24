package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/failclass"
	"prism-2api/internal/groups"
	"prism-2api/internal/persist"
	"prism-2api/internal/secret"
)

func TestLoadOldTokenJSONDefaults(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"accessToken":"aaa","refreshToken":"bbb","api_key":"sk-test"}`
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("legacy")
	if acc == nil {
		t.Fatal("legacy account not loaded")
	}
	snap := acc.Snapshot()
	if !snap.Enabled {
		t.Fatal("missing enabled must default true")
	}
	if snap.Weight != 1 {
		t.Fatalf("missing weight must default 1, got %d", snap.Weight)
	}
	if snap.ID == "" {
		t.Fatal("id should be generated")
	}
}

func TestPersistSchedulingFieldsAndCooldown(t *testing.T) {
	mem := persist.NewMemory()
	if err := mem.SaveDoc(persist.KindAccount, "a", []byte(`{"accessToken":"aaa","refreshToken":"bbb","api_key":"sk-test"}`)); err != nil {
		t.Fatal(err)
	}
	p, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	if acc == nil {
		t.Fatal("missing account")
	}
	acc.mu.Lock()
	acc.priority = 7
	acc.weight = 4
	acc.groups = []string{"team"}
	acc.mu.Unlock()

	acc.MarkFailure(failclass.Result{Class: failclass.RateLimit, Cool: true, Switch: true}, "")
	snap := acc.Snapshot()
	if snap.Disabled || snap.FailClass != string(failclass.RateLimit) {
		t.Fatalf("failure records class but must not disable/cool: %+v", snap)
	}
	if _, err := p.Next(); err != nil {
		t.Fatalf("cooldown disabled: account must stay selectable: %v", err)
	}

	// T3.1：完成路径只标记脏，落盘异步；重开读前显式刷盘，断言不动。
	p.Flush()
	p2, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc2 := p2.Get("a")
	if acc2 == nil {
		t.Fatal("reload missing")
	}
	snap2 := acc2.Snapshot()
	if snap2.Priority != 7 || snap2.Weight != 4 || len(snap2.Groups) != 1 || snap2.Groups[0] != "team" {
		t.Fatalf("scheduling fields lost: %+v", snap2)
	}
	if snap2.FailClass != string(failclass.RateLimit) {
		t.Fatalf("fail class should persist for visibility: %+v", snap2)
	}
	if snap2.CooldownUntil != 0 {
		t.Fatalf("cooldown must not persist/restore anymore: %+v", snap2)
	}

	acc2.MarkSuccess()
	if _, err := p2.Next(); err != nil {
		t.Fatalf("after success should be available: %v", err)
	}
}

func TestCanceledDoesNotCool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	acc.MarkFailure(failclass.Result{Class: failclass.Canceled, Cool: false}, "")
	acc.MarkFailure(failclass.Result{Class: failclass.Region, Cool: false}, "")
	acc.MarkFailure(failclass.Result{Class: failclass.BadRequest, Cool: false}, "")
	if acc.Snapshot().Disabled {
		t.Fatal("request-fault/cancel must not cool")
	}
}

func TestRPMOneUseNotReady(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	acc.nowFn = func() time.Time { return now }
	if err := p.SetRPM("a", 1); err != nil {
		t.Fatal(err)
	}
	if !acc.Ready("", now, true) {
		t.Fatal("rpm=1 unused should be ready")
	}
	acc.TouchUse()
	if acc.Ready("", now, true) {
		t.Fatal("rpm=1 after one use must not be ready")
	}
	if acc.Ready("", now.Add(59*time.Second), true) {
		t.Fatal("still inside 60s window")
	}
	if !acc.Ready("", now.Add(time.Minute), true) {
		t.Fatal("sliding window should release after 60s")
	}
}

func TestDailyLimitUTCReset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	day1 := time.Date(2026, 8, 18, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 19, 0, 0, 1, 0, time.UTC)
	acc.nowFn = func() time.Time { return day1 }
	acc.SetDailyMax(1)
	acc.RecordUse()
	if acc.Ready("", day1, true) {
		t.Fatal("daily_max=1 should block same UTC day")
	}
	if !acc.Ready("", day2, true) {
		t.Fatal("UTC calendar day should reset the daily counter")
	}
}

func TestGroupRenameUpdatesAccountGroups(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetGroups("a", []string{"old", "keep"}); err != nil {
		t.Fatal(err)
	}
	store, err := groups.Load(groups.DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	store.SetAccountRenamer(p)
	if _, err := store.Create("old", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("keep", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	got := p.Get("a").Groups()
	if len(got) != 2 || got[0] != "new" || got[1] != "keep" {
		t.Fatalf("groups after rename = %v", got)
	}
	if err := store.SetGroups(p.Get("a"), []string{"ghost"}); err == nil {
		t.Fatal("SetGroups must reject unknown names")
	}
	if err := store.SetGroups(p.Get("a"), []string{"new"}); err != nil {
		t.Fatal(err)
	}
	if g := p.Get("a").Groups(); len(g) != 1 || g[0] != "new" {
		t.Fatalf("validated set = %v", g)
	}
}

func TestPersistEncryptsTokens(t *testing.T) {
	secret.Configure("test-encrypt-key")
	t.Cleanup(func() { secret.Configure("") })
	mem := persist.NewMemory()
	p, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("enc", "sk-plain-secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := mem.LoadDoc(persist.KindAccount, "enc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-plain-secret") {
		t.Fatalf("token not encrypted: %s", raw)
	}
	p2, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p2.Get("enc")
	if acc == nil || acc.Tokens.APIKey() != "sk-plain-secret" {
		t.Fatal("reload did not decrypt api key")
	}
}

func TestDailyCountPersistsAcrossReload(t *testing.T) {
	mem := persist.NewMemory()
	if err := mem.SaveDoc(persist.KindAccount, "a", []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`)); err != nil {
		t.Fatal(err)
	}
	p, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	day := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	acc.nowFn = func() time.Time { return day }
	acc.SetDailyMax(5)
	acc.RecordUse()
	acc.RecordUse()

	// T3.1：完成路径只标记脏，落盘异步；重开读前显式刷盘，断言不动。
	p.Flush()
	p2, err := Open(mem, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc2 := p2.Get("a")
	if acc2 == nil {
		t.Fatal("missing")
	}
	acc2.nowFn = func() time.Time { return day }
	acc2.SetDailyMax(5)
	if acc2.dailyCount != 2 || acc2.dailyDay != "2026-08-18" {
		t.Fatalf("daily not persisted: count=%d day=%s", acc2.dailyCount, acc2.dailyDay)
	}
	acc2.RecordUse()
	acc2.RecordUse()
	acc2.RecordUse()
	if acc2.Ready("", day, true) {
		t.Fatal("daily_max=5 after 5 uses should block")
	}
}

func TestSanitizeName(t *testing.T) {
	if got := SanitizeName("JillArnemann60813@outlook.com"); got != "JillArnemann60813" {
		t.Fatalf("got %q", got)
	}
	if !ValidName(SanitizeName("VeolaMcnier405942@outlook.com")) {
		t.Fatal("expected valid")
	}
	if ValidName("a@b") {
		t.Fatal("email must not be a valid name")
	}
}

func TestRememberUsageAutoDisableAndRecover(t *testing.T) {
	p, err := New(t.TempDir(), "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("spent", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := p.Get("spent")
	api, auto := 100.0, 1.6
	acc.RememberUsage(adapter.UsageSnapshot{APIPercentUsed: &api, AutoPercentUsed: &auto}, "")
	snap := acc.Snapshot()
	if snap.Enabled || snap.DisableReason != "quota_api" {
		t.Fatalf("expected quota_api disable, got %+v", snap)
	}

	api = 40
	acc.RememberUsage(adapter.UsageSnapshot{APIPercentUsed: &api, AutoPercentUsed: &auto}, "")
	if !acc.Snapshot().Enabled || acc.Snapshot().DisableReason != "" {
		t.Fatalf("expected recover, got %+v", acc.Snapshot())
	}

	off := false
	acc.ApplyAdminPatch(AdminPatch{Enabled: &off})
	acc.RememberUsage(adapter.UsageSnapshot{APIPercentUsed: &api, AutoPercentUsed: &auto}, "")
	if acc.Snapshot().Enabled {
		t.Fatal("manual disable must not auto-recover")
	}

	on := true
	acc.ApplyAdminPatch(AdminPatch{Enabled: &on})
	acc.ApplyAdminPatch(AdminPatch{Enabled: &off})
	api = 100
	acc.RememberUsage(adapter.UsageSnapshot{APIPercentUsed: &api, AutoPercentUsed: &auto}, "")
	if acc.Snapshot().DisableReason != "quota_api" {
		t.Fatalf("disabled+full quota should stamp reason, got %+v", acc.Snapshot())
	}
}

func TestLoadDisablesFullQuota(t *testing.T) {
	dir := t.TempDir()
	raw := `{"accessToken":"aaa","refreshToken":"bbb","api_key":"sk-test","cursor_usage":{"api_percent_used":100,"auto_percent_used":1.2}}`
	if err := os.WriteFile(filepath.Join(dir, "spent.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("spent")
	if acc == nil {
		t.Fatal("missing")
	}
	snap := acc.Snapshot()
	if snap.Enabled || snap.DisableReason != "quota_api" {
		t.Fatalf("expected load-time disable, got %+v", snap)
	}
}

func TestFindDuplicateByEmailAndToken(t *testing.T) {
	p, err := New(t.TempDir(), "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("acc1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := p.Get("acc1")
	email := "jill@outlook.com"
	acc.RememberUsage(adapter.UsageSnapshot{Email: email, WorkOSID: "user_01JILL"}, "")
	if dup := p.FindDuplicate("other", email, "", ""); dup == nil || dup.Name != "acc1" {
		t.Fatal("email should match")
	}
	if p.FindDuplicate("acc1", email, "", "") != nil {
		t.Fatal("except self")
	}
}

func TestAccountConcurrencyUnlimitedByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	// 本上游没有速率限制：默认不设单号并发上限。
	if snap := acc.Snapshot(); snap.ConcurrencyLimit != 0 {
		t.Fatalf("default concurrency limit = %d, want 0 (unlimited)", snap.ConcurrencyLimit)
	}
	for i := 0; i < 200; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d must succeed when unlimited", i+1)
		}
	}
	if !acc.Ready("", time.Now(), true) {
		t.Fatal("uncapped account must stay Ready while in flight")
	}
	for i := 0; i < 200; i++ {
		acc.Release()
	}
	if acc.Inflight() != 0 {
		t.Fatalf("inflight after full release = %d, want 0", acc.Inflight())
	}
}

func TestAccountConcurrencyGateWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	p.SetConcurrencyResolver(func() (int, int) { return 10, 0 })
	if snap := acc.Snapshot(); snap.ConcurrencyLimit != 10 {
		t.Fatalf("configured limit = %d, want 10", snap.ConcurrencyLimit)
	}
	for i := 0; i < 10; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d should succeed within limit 10", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("11th acquire must fail at limit 10")
	}
	if snap := acc.Snapshot(); snap.Inflight != 10 {
		t.Fatalf("inflight = %d, want 10", snap.Inflight)
	}
	// 满号：Ready 判定不可用（选号会跳过）。
	if acc.Ready("", time.Now(), true) {
		t.Fatal("full account must not be Ready")
	}
	acc.Release()
	if !acc.Acquire() {
		t.Fatal("after release acquire must succeed")
	}
	for i := 0; i < 10; i++ {
		acc.Release()
	}
	if acc.Inflight() != 0 {
		t.Fatalf("inflight after full release = %d, want 0", acc.Inflight())
	}
	if !acc.Ready("", time.Now(), true) {
		t.Fatal("released account must be Ready")
	}
}

func TestAccountConcurrency429Degrade(t *testing.T) {
	p, err := New(t.TempDir(), "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("acc1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := p.Get("acc1")
	// 默认不降级（degraded=0）：429 打到无上限的号上，并发上限仍然是"不限制"。
	p.SetConcurrencyResolver(func() (int, int) { return 0, 0 })
	acc.MarkFailure(failclass.Result{Class: failclass.RateLimit, Cool: true, Switch: true}, "")
	for i := 0; i < 50; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d must succeed when degraded limit is 0 (no downgrade)", i+1)
		}
	}
	for i := 0; i < 50; i++ {
		acc.Release()
	}
	// 配了 degraded 才降级：429 → RateLimit 类 → 上限立即降为 5。
	p.SetConcurrencyResolver(func() (int, int) { return 10, 5 })
	if snap := acc.Snapshot(); !snap.ConcurrencyDegraded || snap.ConcurrencyLimit != 5 {
		t.Fatalf("after 429: degraded=%v limit=%d, want true/5", snap.ConcurrencyDegraded, snap.ConcurrencyLimit)
	}
	for i := 0; i < 5; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d should succeed at degraded limit 5", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("6th acquire must fail at degraded limit 5")
	}
	for i := 0; i < 5; i++ {
		acc.Release()
	}
}

func TestConcurrencyResolverHotUpdate(t *testing.T) {
	p, err := New(t.TempDir(), "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AddAPIKey("acc1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	acc := p.Get("acc1")
	base, degraded := 10, 5
	p.SetConcurrencyResolver(func() (int, int) { return base, degraded })
	for i := 0; i < 10; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d should succeed at base 10", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("11th acquire must fail at base 10")
	}
	for i := 0; i < 10; i++ {
		acc.Release()
	}
	// 热改 base 为 3：立即生效。
	base = 3
	for i := 0; i < 3; i++ {
		if !acc.Acquire() {
			t.Fatalf("acquire %d should succeed after hot update to 3", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("4th acquire must fail after hot update to 3")
	}
	for i := 0; i < 3; i++ {
		acc.Release()
	}
	// 429 降级后取 degraded（热改 degraded 生效）。
	acc.MarkFailure(failclass.Result{Class: failclass.RateLimit, Cool: true, Switch: true}, "")
	degraded = 2
	for i := 0; i < 2; i++ {
		if !acc.Acquire() {
			t.Fatalf("degraded acquire %d should succeed at 2", i+1)
		}
	}
	if acc.Acquire() {
		t.Fatal("degraded 3rd acquire must fail at 2")
	}
	for i := 0; i < 2; i++ {
		acc.Release()
	}
}

func TestAuth403DropsAccessTokenButKeepsBundle(t *testing.T) {
	// 403=上游吊销session：摘 access token（退出选号），但重登凭据束必须保留
	// （保活重登救回的唯一材料）。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"accessToken":"aaa","refreshToken":"bbb"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "ide")
	if err != nil {
		t.Fatal(err)
	}
	acc := p.Get("a")
	acc.MarkFailure(failclass.Result{Class: failclass.Auth, Cool: true}, "")
	if tok := acc.Tokens.TokenLocal(); tok != nil {
		t.Fatal("auth failure must drop access token (not ready)")
	}
	if cur := acc.Tokens.Current(); cur == nil || cur.RefreshToken != "bbb" {
		t.Fatalf("refresh credential bundle must survive, got %+v", cur)
	}
}
