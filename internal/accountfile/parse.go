// Package accountfile 解析账号文件：CSV（带表头，兼容各家导出）/ ---- 分隔行 / 纯文本行。
//
// 支持的两种真实导出（2026-09-17 实测）：
//
//	email,password,totp_secret                                  # gmail_alive_*.csv
//	ID,Email,Password,…,TOTP Secret,…                            # accounts_*.csv（19 列，只取需要的）
//	email----password----totp----proxy                          # 老格式
//
// 表头按"归一化后匹配别名"识别（大小写/空格/下划线/连字符都不敏感），
// 认不出表头时退回按列位置解析（email,password,totp,proxy）——保持向后兼容。
package accountfile

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// Entry 是一条账号记录。
type Entry struct {
	Email    string
	Password string
	TOTP     string
	Proxy    string
	Line     int // 源文件行号（1 起；CSV 里的空行不占记录，行号由 csv.Reader 给出）
}

// Complete 判断这条记录是否具备登录所需的字段。
func (e Entry) Complete() bool { return e.Email != "" && e.Password != "" }

// Name 由邮箱推导池内账号名（acct-<localpart>，只保留 [a-z0-9-_]，≤32 字符）。
func (e Entry) Name(prefix string) string {
	return NameFor(prefix, e.Email)
}

// NameFor 是账号名规则的唯一实现（CLI 与管理端导入共用）。
func NameFor(prefix, email string) string {
	base := strings.SplitN(strings.TrimSpace(email), "@", 2)[0]
	var sb strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	name := strings.Trim(sb.String(), "-")
	if name == "" {
		name = "acct"
	}
	if p := strings.TrimSpace(prefix); p != "" {
		name = p + "-" + name
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

var (
	emailAliases = []string{"email", "mail", "account", "accountemail", "username", "user", "login", "邮箱", "账号"}
	passAliases  = []string{"password", "passwd", "pass", "pwd", "密码"}
	totpAliases  = []string{"totpsecret", "totp", "2fasecret", "2fa", "otpsecret", "otp", "mfasecret", "mfa", "secret", "seed", "2fa密钥", "密钥"}
	proxyAliases = []string{"proxy", "proxyurl", "http_proxy", "出口", "代理"}
)

// normalizeHeader 归一化表头/别名：小写、去掉空格/下划线/连字符/点/星号。
func normalizeHeader(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))) {
		switch r {
		case ' ', '_', '-', '.', '*', '\t', '\u00a0':
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func matchAlias(header string, aliases []string) bool {
	h := normalizeHeader(header)
	if h == "" {
		return false
	}
	for _, a := range aliases {
		if h == normalizeHeader(a) {
			return true
		}
	}
	// 容错：totp_secret_1 / password2 / email_address 这类带后缀的表头。
	for _, a := range aliases {
		if na := normalizeHeader(a); na != "" && strings.HasPrefix(h, na) {
			return true
		}
	}
	return false
}

// ParseFile 读文件解析（自动识别 CSV/----/纯文本）。
func ParseFile(path string) ([]Entry, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return ParseReader(fh)
}

// ParseReader 解析内容（逐条读，以便带上真实行号）。
func ParseReader(r io.Reader) ([]Entry, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.LazyQuotes = true
	var records [][]string
	var lines []int
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析文件失败: %w", err)
		}
		line, _ := cr.FieldPos(0)
		records = append(records, rec)
		lines = append(lines, line)
	}
	return parseRecords(records, lines)
}

// ParseRecords 从已切分的 CSV 记录解析（管理端粘贴文本走这里；行号缺失时按序号）。
func ParseRecords(records [][]string) ([]Entry, error) {
	lines := make([]int, len(records))
	for i := range records {
		lines[i] = i + 1
	}
	return parseRecords(records, lines)
}

func parseRecords(records [][]string, lines []int) ([]Entry, error) {
	// 先找表头（可能前面有空行/注释行）。
	start, colEmail, colPass, colTOTP, colProxy := -1, -1, -1, -1, -1
	for i, rec := range records {
		if len(rec) == 0 || allBlank(rec) {
			continue
		}
		if len(rec) == 1 {
			cell := strings.TrimSpace(strings.TrimPrefix(rec[0], "\ufeff"))
			if cell == "" || strings.HasPrefix(cell, "#") {
				continue
			}
		}
		if matchesHeader(rec) {
			start = i + 1
			colEmail, colPass, colTOTP, colProxy = headerColumns(rec)
			break
		}
		start = i
		break
	}
	if start < 0 {
		return nil, nil
	}

	var out []Entry
	for i := start; i < len(records); i++ {
		rec := records[i]
		if len(rec) == 0 || allBlank(rec) {
			continue
		}
		// 单列：可能是 ---- 分隔行或纯文本。多列：可能是 "email,password,totp" 无表头。
		if len(rec) == 1 {
			cell := strings.TrimSpace(strings.TrimPrefix(rec[0], "\ufeff"))
			if cell == "" || strings.HasPrefix(cell, "#") {
				continue
			}
			if strings.Contains(cell, "----") {
				rec = strings.Split(cell, "----")
			} else if strings.Contains(cell, ",") {
				rec = strings.Split(cell, ",")
			} else {
				rec = []string{cell}
			}
		}
		line := i + 1
		if i < len(lines) {
			line = lines[i]
		}
		e := Entry{Line: line}
		switch {
		case colEmail >= 0: // 有表头：按列名取
			e.Email = cellAt(rec, colEmail)
			e.Password = cellAt(rec, colPass)
			e.TOTP = cellAt(rec, colTOTP)
			e.Proxy = cellAt(rec, colProxy)
		default: // 无表头：按位置
			e.Email = cellAt(rec, 0)
			e.Password = cellAt(rec, 1)
			e.TOTP = cellAt(rec, 2)
			e.Proxy = cellAt(rec, 3)
		}
		e.Email = strings.TrimSpace(e.Email)
		if e.Email == "" {
			continue
		}
		if !strings.Contains(e.Email, "@") && !looksLikeToken(e.Email) {
			continue // 既不是邮箱也不是 token，跳过（例如分隔行）
		}
		out = append(out, e)
	}
	return out, nil
}

// LooksLikeHeader 判断一行是不是账号表头（供调用方决定要不要走 CSV 路径）。
func LooksLikeHeader(rec []string) bool { return matchesHeader(rec) }

// matchesHeader 判断第一行是不是表头：至少要有 email 列，或有 ≥2 个已知列名。
func matchesHeader(rec []string) bool {
	hits := 0
	for _, c := range rec {
		switch {
		case matchAlias(c, emailAliases), matchAlias(c, passAliases),
			matchAlias(c, totpAliases), matchAlias(c, proxyAliases):
			hits++
		}
	}
	if hits == 0 {
		return false
	}
	for _, c := range rec {
		if matchAlias(c, emailAliases) {
			return true
		}
	}
	return hits >= 2
}

func headerColumns(rec []string) (email, pass, totp, proxy int) {
	email, pass, totp, proxy = -1, -1, -1, -1
	for i, c := range rec {
		switch {
		case email < 0 && matchAlias(c, emailAliases):
			email = i
		case pass < 0 && matchAlias(c, passAliases):
			pass = i
		case totp < 0 && matchAlias(c, totpAliases):
			totp = i
		case proxy < 0 && matchAlias(c, proxyAliases):
			proxy = i
		}
	}
	return
}

func cellAt(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func allBlank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// looksLikeToken 判断是不是已登录凭据（纯 token 导入仍走老路径）。
func looksLikeToken(s string) bool {
	return strings.HasPrefix(s, "eyJ") || strings.Contains(s, "prism_oai_access_token=") || strings.Contains(s, "prism_session_token=")
}
