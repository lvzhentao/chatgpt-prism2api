package main

import (
	"os"
	"path/filepath"
	"testing"

	"prism-2api/internal/auth"
	"prism-2api/internal/pool"
)

// 账号文件解析是登录批量的入口，格式错一个字段就会把密码/TOTP 张冠李戴。
func TestCollectAccountsFileFormats(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "a.csv")
	content := "email,password,totp_secret,proxy\n" +
		"a@b.com,pw1,SECRET1,http://p:1\n" +
		"# comment line\n" +
		"c@d.com,pw2\n"
	if err := os.WriteFile(csvPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := collectAccounts(options{file: csvPath})
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 accounts (header + comment skipped), got %d: %+v", len(got), got)
	}
	if got[0].Email != "a@b.com" || got[0].Password != "pw1" || got[0].TOTP != "SECRET1" || got[0].Proxy != "http://p:1" {
		t.Fatalf("row 1 parsed wrong: %+v", got[0])
	}
	if got[1].Email != "c@d.com" || got[1].Password != "pw2" || got[1].TOTP != "" {
		t.Fatalf("row 2 parsed wrong: %+v", got[1])
	}

	dashPath := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(dashPath, []byte("x@y.com----pw3----SECRET3----socks5://localhost:1080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = collectAccounts(options{file: dashPath})
	if err != nil {
		t.Fatalf("dash parse: %v", err)
	}
	if len(got) != 1 || got[0].Email != "x@y.com" || got[0].TOTP != "SECRET3" || got[0].Proxy != "socks5://localhost:1080" {
		t.Fatalf("---- 行解析错误: %+v", got)
	}
}

func TestCollectAccountsRejectsMissingPassword(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.csv")
	if err := os.WriteFile(p, []byte("a@b.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectAccounts(options{file: p}); err == nil {
		t.Fatal("缺密码的行必须报错，否则会拿空密码去登录")
	}
}

// 回归：导入即停用（等登录），登录成功必须放回调度。
// 曾漏掉后半个动作 → 100 个账号登录完成，却被调度器的 Enabled 过滤掉，池子照旧空转。
func TestStoreAccountEnablesImportedAccount(t *testing.T) {
	p, err := pool.New("", "http://127.0.0.1:1", "http://127.0.0.1:1", "0", "web")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	acc := account{Email: "someone.here@example.com", Password: "pw", TOTP: "SECRET"}
	name := accountName("acct", acc.Email)

	if err := storeImport(p, name, acc, options{}); err != nil {
		t.Fatalf("storeImport: %v", err)
	}
	if a := p.Get(name); a == nil || a.Enabled() {
		t.Fatal("导入后必须是停用（否则会被保活当成待续期而批量登录）")
	}

	if err := storeAccount(p, name, &auth.Token{AccessToken: "tok", RefreshToken: "rt"}, options{}); err != nil {
		t.Fatalf("storeAccount: %v", err)
	}
	a := p.Get(name)
	if a == nil || !a.Enabled() {
		t.Fatal("登录成功后必须启用，否则调度器 pick 会跳过它")
	}
	if !a.Snapshot().LoggedIn {
		t.Fatal("登录成功后必须写入令牌")
	}
}

// 指纹文件必须和侧车（管理台/保活重登）落在同一处、同一命名：CLI 自带一套命名会让
// 同一个账号留下两份互不认的 storage_state，重登复用不到，还落在容器临时层里。
func TestLoginRequestSharesStorageStateWithSidecar(t *testing.T) {
	acc := account{Email: "a@b.com", Password: "pw", TOTP: "SECRET", Proxy: "http://p:1"}
	opt := options{backend: "chromium", headful: true}

	got := loginRequest(opt, acc, "", "acct-a")
	if got.StorageStateIn != "" || got.StorageStateOut != "" {
		t.Fatalf("留空 state-dir 时不应自带路径（应交给侧车按 PRISM_LOGIN_STATE_DIR 命名）: in=%q out=%q",
			got.StorageStateIn, got.StorageStateOut)
	}
	if got.Email != "a@b.com" || got.Password != "pw" || got.TOTPSecret != "SECRET" || got.Proxy != "http://p:1" {
		t.Fatalf("凭据字段串了: %+v", got)
	}
	if got.Backend != "chromium" || got.Headless {
		t.Fatalf("后端/有头标志串了: %+v", got)
	}

	got = loginRequest(opt, acc, "/data/login-state", "acct-a")
	if got.StorageStateIn != "/data/login-state/acct-a.json" || got.StorageStateOut != got.StorageStateIn {
		t.Fatalf("显式 state-dir 未生效: %+v", got)
	}
}

// 入库账号名要满足内核的 nameRe（[a-zA-Z0-9_-]{1,32}）。
func TestAccountNameSanitized(t *testing.T) {
	cases := map[string]string{
		"barbara.edwards+1@gmail.com": "acct-barbara-edwards-1",
		"a_b-c@x.com":                 "acct-a_b-c",
		"??@x.com":                    "acct-acct",
	}
	for email, want := range cases {
		if got := accountName("acct", email); got != want {
			t.Fatalf("accountName(%q) = %q, want %q", email, got, want)
		}
	}
	if got := accountName("", "longlocalpartxxxxxxxxxxxxxxxxxxxxxxxxxxx@x.com"); len(got) > 32 {
		t.Fatalf("name too long: %d", len(got))
	}
}
