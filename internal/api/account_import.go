package api

import (
	"encoding/csv"
	"strings"

	"prism-2api/internal/accountfile"
	siteadapter "prism-2api/internal/adapter/prism"
	"prism-2api/internal/adminapi"
)

// ============================================================
// CSV 导入（账号密码 + TOTP）→ 号池「待登录」条目
//
// 用户手上的两种导出：
//   email,password,totp_secret                                  # gmail_alive_*.csv
//   ID,Email,Password,…,TOTP Secret,…                            # accounts_*.csv
// 都交给 internal/accountfile 解析（表头映射 + 位置兜底），
// 这里只负责把它变成内核的 AccountImport：凭据束写进 RefreshToken（加密落库），
// 账号先停用（没有 token，登录成功后再启用）——登录由「选中 → 登录」动作完成。
// ============================================================

// looksLikeCredentialCSV 判断导入文本是不是「邮箱/密码/2FA」CSV（不是纯 token 行）。
func looksLikeCredentialCSV(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
		return false
	}
	first := firstNonEmptyLine(text)
	if first == "" {
		return false
	}
	if strings.Contains(first, "----") {
		return false // 老格式交给内核解析器
	}
	if !strings.Contains(first, ",") {
		return false
	}
	rec, err := csv.NewReader(strings.NewReader(first)).Read()
	if err != nil {
		return false
	}
	return accountfile.LooksLikeHeader(rec)
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")); s != "" && !strings.HasPrefix(s, "#") {
			return s
		}
	}
	return ""
}

// credentialCSVToImports 把 CSV 文本转成待登录账号（解析不了返回 ok=false）。
func credentialCSVToImports(text, namePrefix string) ([]adminapi.AccountImport, bool) {
	recs, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		return nil, false
	}
	entries, err := accountfile.ParseRecords(recs)
	if err != nil || len(entries) == 0 {
		return nil, false
	}
	out := make([]adminapi.AccountImport, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		email := strings.TrimSpace(e.Email)
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		in := adminapi.AccountImport{
			Name:  e.Name(namePrefix),
			Email: email,
		}
		if e.Password != "" {
			// 凭据束（邮箱+密码+TOTP+代理）：内核 keepalive 到点会自动重登。
			in.RefreshToken = siteadapter.BundleForImport(email, e.Password, e.TOTP, e.Proxy)
		}
		if p := strings.TrimSpace(e.Proxy); p != "" {
			in.ProxyURL = &p
		}
		disabled := true
		in.Enabled = &disabled
		out = append(out, in)
	}
	return out, len(out) > 0
}
