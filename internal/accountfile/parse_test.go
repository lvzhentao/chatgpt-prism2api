package accountfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 两种真实导出的表头必须都能解析出 (email,password,totp)——列顺序不同、列名不同。
func TestParseRealWorldExports(t *testing.T) {
	cases := []struct {
		name string
		csv  string
		want Entry
	}{
		{
			name: "gmail_alive_* 三列",
			csv:  "email,password,totp_secret\nrut***@gmail.com,Passw0rd!!,FUM7TWPZ4P52OWWTYBGV474Y75WH\n",
			want: Entry{Email: "rut***@gmail.com", Password: "Passw0rd!!", TOTP: "FUM7TWPZ4P52OWWTYBGV474Y75WH"},
		},
		{
			name: "accounts_*.csv 十九列（列顺序不同）",
			csv: "ID,Email,Password,Client ID,Account ID,Workspace ID,Access Token,Refresh Token," +
				"ID Token,Session Token,Cookies,TOTP Secret,TOTP Recovery Codes,MFA Enabled,Email Service,Status\n" +
				"3029,a@icloud.com,Pw123456,app_X8zY,x,y,eyJhbGciOi,tok,id,sess,cookie,DKD2H3L52FFBYNYZAZGEX7R5VRN7,,yes,smsbower\n",
			want: Entry{Email: "a@icloud.com", Password: "Pw123456", TOTP: "DKD2H3L52FFBYNYZAZGEX7R5VRN7"},
		},
		{
			name: "无表头按位置",
			csv:  "a@b.com,pw1,TOTPSECRET\n",
			want: Entry{Email: "a@b.com", Password: "pw1", TOTP: "TOTPSECRET"},
		},
		{
			name: "---- 分隔（含代理）",
			csv:  "a@b.com----pw1----TOTPSECRET----http://user:pass@host:3010\n",
			want: Entry{Email: "a@b.com", Password: "pw1", TOTP: "TOTPSECRET", Proxy: "http://user:pass@host:3010"},
		},
		{
			name: "表头带 BOM/空格/大小写",
			csv:  "\ufeff Email , Password , TOTP Secret \n a@b.com , pw1 , SECRET \n",
			want: Entry{Email: "a@b.com", Password: "pw1", TOTP: "SECRET"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseReader(strings.NewReader(c.csv))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1: %+v", len(got), got)
			}
			if got[0].Email != c.want.Email || got[0].Password != c.want.Password ||
				got[0].TOTP != c.want.TOTP || got[0].Proxy != c.want.Proxy {
				t.Fatalf("got %+v, want %+v", got[0], c.want)
			}
			if !got[0].Complete() {
				t.Fatalf("entry should be complete: %+v", got[0])
			}
		})
	}
}

// 表头认错会把整列当成密码 → 登录全失败，所以这里钉住"不误判"。
func TestParseRecordsSkipsNoiseWithoutMisreading(t *testing.T) {
	in := "\n# 注释行\nemail,password,totp_secret\na@b.com,pw1,S1\n\nc@d.com,pw2,S2\n"
	got, err := ParseReader(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[1].Email != "c@d.com" || got[1].TOTP != "S2" {
		t.Fatalf("second entry = %+v", got[1])
	}
	// 行号要指向源文件（诊断用）
	if got[0].Line != 4 {
		t.Fatalf("first entry line = %d, want 4", got[0].Line)
	}
}

func TestParseEntryNameRules(t *testing.T) {
	cases := []struct{ email, prefix, want string }{
		{"User.Name+tag@gmail.com", "acct", "acct-user-name-tag"},
		{"a@b.com", "", "a"},
		{"@b.com", "acct", "acct-acct"},
		{"verylonglocalpartthatexceedsthirtytwochars@x.com", "acct", "acct-verylonglocalpartthatexceed"},
	}
	for _, c := range cases {
		if got := NameFor(c.prefix, c.email); got != c.want {
			t.Errorf("NameFor(%q,%q) = %q, want %q", c.prefix, c.email, got, c.want)
		}
	}
}

// 真实文件（若存在）跑一遍，确认行数与列映射。
func TestParseRealFilesIfPresent(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, "csv/gmail_alive_50_5_email_password_2fa.csv"),
		filepath.Join(home, "Downloads/accounts_20260917_043858.csv"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("样本文件不存在：%s", p)
		}
		entries, err := ParseFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s: 解析出 0 条", p)
		}
		for i, e := range entries {
			if !e.Complete() {
				t.Fatalf("%s 第 %d 条缺字段: %+v", p, i, e)
			}
			if !strings.Contains(e.Email, "@") {
				t.Fatalf("%s 第 %d 条 email 不像邮箱: %q", p, i, e.Email)
			}
		}
		t.Logf("%s → %d 条", filepath.Base(p), len(entries))
	}
}
