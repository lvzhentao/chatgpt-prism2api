package admin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"prism-2api/internal/persist"
)

func TestLoadOrCreateDefaultMustChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	rc, err := LoadOrCreate(path, "https://api2.cursor.sh", "https://cursor.com", "sk-x", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.MustChange() {
		t.Fatal("first start must persist must_change_password")
	}
	if !rc.CheckPassword(DefaultAdminPassword) {
		t.Fatal("default password")
	}
}

func TestRejectDefaultPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	rc, err := LoadOrCreate(path, "https://api2.cursor.sh", "https://cursor.com", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.ChangePassword(DefaultAdminPassword); !errors.Is(err, ErrDefaultAdminPassword) {
		t.Fatalf("want ErrDefaultAdminPassword, got %v", err)
	}
	_, _, err = rc.Update(map[string]any{"admin_password": DefaultAdminPassword})
	if !errors.Is(err, ErrDefaultAdminPassword) {
		t.Fatalf("update want ErrDefaultAdminPassword, got %v", err)
	}
}

func TestChangePasswordClearsMustChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	rc, err := LoadOrCreate(path, "https://api2.cursor.sh", "https://cursor.com", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.ChangePassword("n3w-pass"); err != nil {
		t.Fatal(err)
	}
	if rc.MustChange() {
		t.Fatal("flag should clear")
	}
	if !rc.CheckPassword("n3w-pass") {
		t.Fatal("new password")
	}
}

func TestDetectDefaultPasswordOnReload(t *testing.T) {
	mem := persist.NewMemory()
	rc, err := Open(mem, "https://api2.cursor.sh", "https://cursor.com", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	rc.MustChangePassword = false
	if err := rc.Save(); err != nil {
		t.Fatal(err)
	}
	rc2, err := Open(mem, "https://api2.cursor.sh", "https://cursor.com", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if !rc2.MustChange() {
		t.Fatal("reload must detect admin123 and set flag")
	}
}

func TestLoadOrCreateYAMLSibling(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yml, []byte("admin_username: yadmin\napi_base_url: https://example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, err := LoadOrCreate(filepath.Join(dir, "config.json"), "https://api2.cursor.sh", "https://cursor.com", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if rc.AdminUsername != "yadmin" {
		t.Fatalf("username %q", rc.AdminUsername)
	}
	if rc.APIBaseURL != "https://example.test" {
		t.Fatalf("api_base_url %q", rc.APIBaseURL)
	}
}

func TestPromptDefaultsToInject(t *testing.T) {
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	if !rc.PromptEnabled() {
		t.Fatal("默认应注入")
	}
	if rc.PromptText() != "" {
		t.Fatalf("默认文案应为空（回内置默认），got %q", rc.PromptText())
	}
	if !rc.IdentityGuardOn() {
		t.Fatal("身份闸默认应开")
	}
}

func TestPromptEnvSeeding(t *testing.T) {
	t.Setenv("PRISM_SYSTEM_PROMPT", "off")
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	if rc.PromptEnabled() {
		t.Fatal("PRISM_SYSTEM_PROMPT=off 应播种为关")
	}
}

func TestPromptEnvTextSeeding(t *testing.T) {
	t.Setenv("PRISM_SYSTEM_PROMPT", `第一行\n第二行`)
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	if got := rc.PromptText(); got != "第一行\n第二行" {
		t.Fatalf("env 文案应播种并还原换行，got %q", got)
	}
}

func TestPromptUpdateRoundTrip(t *testing.T) {
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	snap, restart, err := rc.Update(map[string]any{
		"system_prompt":          "自定义指令",
		"system_prompt_mode":     "off",
		"identity_guard_enabled": false,
		"identity_answer_text":   "自定义答句",
	})
	if err != nil {
		t.Fatal(err)
	}
	if restart {
		t.Fatal("注入字段应即时生效，不需重启")
	}
	if rc.PromptEnabled() {
		t.Fatal("mode=off 后 PromptEnabled 应为 false")
	}
	if snap["system_prompt"] != "自定义指令" || snap["system_prompt_mode"] != "off" ||
		snap["identity_guard_enabled"] != false || snap["identity_answer_text"] != "自定义答句" {
		t.Fatalf("快照应回显新字段，got %v", snap)
	}
	if _, _, err := rc.Update(map[string]any{"system_prompt_mode": "inject"}); err != nil {
		t.Fatal(err)
	}
	if !rc.PromptEnabled() || rc.PromptText() != "自定义指令" {
		t.Fatalf("切回 inject 后文案应生效，got enabled=%v text=%q", rc.PromptEnabled(), rc.PromptText())
	}
}

func TestNoLocalRateLimitByDefault(t *testing.T) {
	// 上游没有速率限制：本地默认不设单号并发上限、429 也不降级。
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	if got := rc.AccountConcurrencyN(); got != 0 {
		t.Fatalf("account_concurrency 默认应为 0（不限制），got %d", got)
	}
	if got := rc.AccountConcurrency429N(); got != 0 {
		t.Fatalf("account_concurrency_429 默认应为 0（不降级），got %d", got)
	}
}

func TestChatConcurrencyKnobIsGone(t *testing.T) {
	// 对话并发不再有本地闸门：管理端配置面里不得再出现 max_in_flight，存过它的老配置也不能报错。
	mem := persist.NewMemory()
	if err := mem.SaveDoc(persist.KindConfig, persist.IDMain, []byte(`{"max_in_flight":64,"account_concurrency":0}`)); err != nil {
		t.Fatal(err)
	}
	rc, err := Open(mem, "https://x", "https://y", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatalf("老配置带 max_in_flight 也应能载入：%v", err)
	}
	if _, ok := rc.Get()["max_in_flight"]; ok {
		t.Fatal("管理端配置面不应再暴露 max_in_flight")
	}
	if _, _, err := rc.Update(map[string]any{"max_in_flight": 64}); err != nil {
		t.Fatalf("老客户端提交 max_in_flight 不应报错（应为惰性忽略）：%v", err)
	}
	if _, ok := rc.Get()["max_in_flight"]; ok {
		t.Fatal("提交后仍不应出现 max_in_flight")
	}
}

func TestAccountConcurrencyZeroIsSettable(t *testing.T) {
	// 老库里残留的 10/5 必须能被管理端改回 0：patch 校验不得把 0 当非法值静默丢弃。
	rc := Default("https://x", "https://y", "", "127.0.0.1:0", true)
	if _, _, err := rc.Update(map[string]any{"account_concurrency": 10, "account_concurrency_429": 5}); err != nil {
		t.Fatal(err)
	}
	if got := rc.AccountConcurrencyN(); got != 10 {
		t.Fatalf("显式配 10 应生效，got %d", got)
	}
	if _, _, err := rc.Update(map[string]any{"account_concurrency": 0, "account_concurrency_429": 0}); err != nil {
		t.Fatal(err)
	}
	if got := rc.AccountConcurrencyN(); got != 0 {
		t.Fatalf("改回 0 应生效（= 不限制），got %d", got)
	}
	if got := rc.AccountConcurrency429N(); got != 0 {
		t.Fatalf("改回 0 应生效（= 不降级），got %d", got)
	}
}

func TestAccountConcurrencyZeroSurvivesLoad(t *testing.T) {
	// 存量配置里显式的 0 不得被载入归一化回退成 10/5。
	mem := persist.NewMemory()
	doc := []byte(`{"account_concurrency":0,"account_concurrency_429":0}`)
	if err := mem.SaveDoc(persist.KindConfig, persist.IDMain, doc); err != nil {
		t.Fatal(err)
	}
	rc, err := Open(mem, "https://x", "https://y", "", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.AccountConcurrencyN(); got != 0 {
		t.Fatalf("载入后 account_concurrency 应为 0，got %d", got)
	}
	if got := rc.AccountConcurrency429N(); got != 0 {
		t.Fatalf("载入后 account_concurrency_429 应为 0，got %d", got)
	}
}
