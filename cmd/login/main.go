// prism-server login —— 用账号密码 + TOTP 登录 Prism，产出并入库凭据。
//
// 为什么是独立命令：登录要跑真浏览器（auth.openai.com 对非浏览器客户端一律回
// Cloudflare JS 挑战），和网关进程生命周期不同——网关不该背着浏览器运行时。
//
// 用法：
//
//	# 单条：登录并直接入库
//	prism-server-login --email a@b.com --password *** --totp BASE32 \
//	    --database-url postgres://... --backend camoufox
//
//	# 批量：CSV/文本，每行 email,password,totp_secret[,proxy]
//	prism-server-login --file accounts.csv --database-url postgres://... --concurrency 2
//
//	# 只验证不入库（打印凭据摘要）
//	prism-server-login --email ... --password ... --totp ... --dry-run
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"prism-2api/internal/accountfile"
	siteadapter "prism-2api/internal/adapter/prism"
	"prism-2api/internal/auth"
	"prism-2api/internal/pg"
	"prism-2api/internal/pool"
)

type options struct {
	email       string
	password    string
	totp        string
	proxy       string
	file        string
	backend     string
	headful     bool
	dryRun      bool
	concurrency int
	databaseURL string
	baseURL     string
	clientType  string
	namePrefix  string
	overwrite   bool
	stateDir    string
	only        string
	limit       int
	importOnly  bool
	status      string
}

func main() {
	var opt options
	flag.StringVar(&opt.email, "email", "", "账号邮箱")
	flag.StringVar(&opt.password, "password", "", "账号密码")
	flag.StringVar(&opt.totp, "totp", "", "TOTP secret（Base32，可选：没绑 2FA 就不填）")
	flag.StringVar(&opt.proxy, "proxy", "", "出口代理，如 http://user:pass@host:port")
	flag.StringVar(&opt.file, "file", "", "批量文件：每行 email,password,totp_secret[,proxy]（也支持 ---- 分隔）")
	flag.StringVar(&opt.backend, "backend", "camoufox", "浏览器后端：camoufox | chromium（本地开发）")
	flag.BoolVar(&opt.headful, "headful", true, "开有头浏览器（headless 过不了 Cloudflare 挑战）")
	flag.BoolVar(&opt.dryRun, "dry-run", false, "只登录验证，不入库")
	flag.IntVar(&opt.concurrency, "concurrency", 1, "批量并发（浏览器很重，建议 1~3）")
	flag.StringVar(&opt.databaseURL, "database-url", firstEnv("WEB2API_DATABASE_URL", "DATABASE_URL"), "PostgreSQL URL（入库时必填）")
	flag.StringVar(&opt.baseURL, "endpoint", "https://prism.openai.com", "Prism 站点地址")
	flag.StringVar(&opt.clientType, "client-type", "web", "客户端类型标记")
	flag.StringVar(&opt.namePrefix, "name-prefix", "acct", "入库账号名前缀（后缀取邮箱本地部分）")
	flag.BoolVar(&opt.overwrite, "overwrite", false, "同名账号已存在时覆盖")
	flag.StringVar(&opt.stateDir, "state-dir", "", "浏览器 storage_state 目录（留空=由侧车按 PRISM_LOGIN_STATE_DIR 落盘，与管理台重登共用指纹）")
	flag.StringVar(&opt.only, "only", "", "只处理这些账号（邮箱或账号名，逗号/空格分隔）")
	flag.IntVar(&opt.limit, "limit", 0, "最多处理多少个（0=不限）")
	flag.BoolVar(&opt.importOnly, "import-only", false, "只导入（写凭据束、账号停用），不登录")
	flag.StringVar(&opt.status, "status", "", "从号池里按状态选：pending(未登录) / all")
	flag.Parse()

	accounts, err := collectAccounts(opt)
	if err != nil {
		log.Fatalf("参数错误: %v", err)
	}
	// --status 从号池里选账号（不读文件）：pending=未登录（导入过的 CSV 账号就停在这），all=全部。
	if st := strings.TrimSpace(opt.status); st != "" {
		if opt.file != "" || opt.email != "" {
			log.Fatal("--status 与 --file/--email 互斥：要么从文件给账号，要么从号池选")
		}
		if strings.TrimSpace(opt.databaseURL) == "" {
			log.Fatal("--status 需要 --database-url（或 WEB2API_DATABASE_URL）")
		}
		if opt.dryRun {
			log.Fatal("--status 与 --dry-run 互斥（dry-run 不连库）")
		}
	}
	if len(accounts) == 0 && strings.TrimSpace(opt.status) == "" {
		fmt.Fprintln(os.Stderr, "没有账号。用 --email/--password/--totp、--file accounts.csv，或 --status pending")
		flag.Usage()
		os.Exit(2)
	}

	var p *pool.Pool
	needPool := !opt.dryRun || strings.TrimSpace(opt.status) != ""
	if needPool {
		if strings.TrimSpace(opt.databaseURL) == "" {
			log.Fatal("--database-url（或 WEB2API_DATABASE_URL）必填；只想验证加 --dry-run")
		}
		p, err = openPool(opt)
		if err != nil {
			log.Fatalf("连接数据库失败: %v", err)
		}
	}
	if p != nil {
		if st := strings.TrimSpace(opt.status); st != "" {
			accounts = accountsFromPool(p, st)
			if len(accounts) == 0 {
				fmt.Fprintf(os.Stderr, "号池里没有符合条件的账号（status=%s）\n", st)
				os.Exit(1)
			}
		}
		accounts = selectAccounts(accounts, opt.only, opt.limit)
		if len(accounts) == 0 {
			fmt.Fprintln(os.Stderr, "--only/--limit 之后没有账号可处理")
			os.Exit(1)
		}
	} else {
		accounts = selectAccounts(accounts, opt.only, opt.limit)
	}

	if opt.importOnly {
		okN := 0
		for _, acc := range accounts {
			name := accountName(opt.namePrefix, acc.Email)
			if err := storeImport(p, name, acc, opt); err != nil {
				fmt.Printf("  ✗ %-40s %v\n", acc.Email, err)
				continue
			}
			okN++
			fmt.Printf("  ✓ %-40s 已导入（待登录）\n", acc.Email)
		}
		fmt.Printf("\n导入 %d / %d\n", okN, len(accounts))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// storage_state：显式给了 --state-dir 才自己拼「<池名>.json」，留空则交给侧车按
	// PRISM_LOGIN_STATE_DIR 用「<邮箱>.json」统一落盘（容器里两侧都挂 /data/login-state）。
	// 管理台与保活重登走的就是侧车那条规则：CLI 另起一套命名会让同一个账号留下两份
	// 互不认的设备指纹，重登复用不到，而且落在容器临时层里、容器一重建就丢。
	stateDir := strings.TrimSpace(opt.stateDir)

	type outcome struct {
		email string
		name  string
		ok    bool
		msg   string
	}
	results := make([]outcome, 0, len(accounts))
	var mu sync.Mutex
	sem := make(chan struct{}, max(1, opt.concurrency))
	var wg sync.WaitGroup

	for _, acc := range accounts {
		acc := acc
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			name := accountName(opt.namePrefix, acc.Email)
			res := outcome{email: acc.Email, name: name}
			token, msg := loginOne(ctx, opt, acc, stateDir, name)
			if token == nil {
				res.msg = msg
			} else if p == nil {
				res.ok = true
				res.msg = fmt.Sprintf("登录成功（dry-run，token %d 字节）", len(token.AccessToken))
			} else if err := storeAccount(p, name, token, opt); err != nil {
				res.msg = "入库失败: " + err.Error()
			} else {
				res.ok = true
				res.msg = "已入库"
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	okCount := 0
	fmt.Println()
	fmt.Println("结果：")
	for _, r := range results {
		mark := "✗"
		if r.ok {
			mark = "✓"
			okCount++
		}
		fmt.Printf("  %s %-40s %s\n", mark, r.email, r.msg)
	}
	fmt.Printf("\n成功 %d / %d\n", okCount, len(results))
	if okCount != len(results) {
		os.Exit(1)
	}
}

type account struct {
	Email    string
	Password string
	TOTP     string
	Proxy    string
	Line     int
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func collectAccounts(opt options) ([]account, error) {
	if strings.TrimSpace(opt.email) != "" {
		if strings.TrimSpace(opt.password) == "" {
			return nil, fmt.Errorf("--password 不能为空")
		}
		return []account{{Email: strings.TrimSpace(opt.email), Password: opt.password, TOTP: strings.TrimSpace(opt.totp), Proxy: strings.TrimSpace(opt.proxy)}}, nil
	}
	if strings.TrimSpace(opt.file) == "" {
		return nil, nil
	}
	entries, err := accountfile.ParseFile(opt.file)
	if err != nil {
		return nil, err
	}
	var out []account
	for _, e := range entries {
		if e.Password == "" {
			return nil, fmt.Errorf("第 %d 行缺密码: %s", e.Line, e.Email)
		}
		out = append(out, account{Email: e.Email, Password: e.Password, TOTP: e.TOTP, Proxy: e.Proxy, Line: e.Line})
	}
	return out, nil
}

// firstEnv 返回第一个非空环境变量。
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// selectAccounts 应用 --only / --limit 选择器（按邮箱或账号名匹配，大小写不敏感）。
func selectAccounts(accounts []account, only string, limit int) []account {
	tokens := strings.FieldsFunc(only, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	if len(tokens) > 0 {
		want := make(map[string]bool, len(tokens))
		for _, tk := range tokens {
			want[strings.ToLower(strings.TrimSpace(tk))] = true
		}
		kept := make([]account, 0, len(accounts))
		for _, a := range accounts {
			if want[strings.ToLower(a.Email)] || want[strings.ToLower(accountName("acct", a.Email))] {
				kept = append(kept, a)
			}
		}
		accounts = kept
	}
	if limit > 0 && len(accounts) > limit {
		accounts = accounts[:limit]
	}
	return accounts
}

func accountName(prefix, email string) string { return accountfile.NameFor(prefix, email) }

// accountsFromPool 按状态从号池里取账号（pending=未登录）。
func accountsFromPool(p *pool.Pool, status string) []account {
	var out []account
	for _, a := range p.Accounts() {
		snap := a.Snapshot()
		switch strings.ToLower(status) {
		case "pending", "not-logged-in", "todo":
			if snap.LoggedIn {
				continue
			}
		case "all", "logged", "any":
		default:
			log.Fatalf("未知 --status %q（可用：pending / all）", status)
		}
		b := siteadapter.ParseCredentialBundle(refreshOf(a))
		if b == nil || !b.CanRelogin() {
			continue // 没有邮箱/密码，登不了
		}
		rec := account{Email: b.Email, Password: b.Password, TOTP: b.TOTP, Proxy: b.Proxy}
		if rec.Email == "" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func refreshOf(a *pool.Account) string {
	if a == nil || a.Tokens == nil {
		return ""
	}
	if tok := a.Tokens.Current(); tok != nil {
		return tok.RefreshToken
	}
	return ""
}

// storeImport 只写凭据束并把账号置为停用（等登录成功再启用）。
func storeImport(p *pool.Pool, name string, acc account, opt options) error {
	if p == nil {
		return fmt.Errorf("没有号池连接")
	}
	if existing := p.Get(name); existing != nil && !opt.overwrite {
		if snap := existing.Snapshot(); snap.LoggedIn {
			return fmt.Errorf("已登录，跳过（加 --overwrite 覆盖）")
		}
	}
	if err := p.SetToken(name, &auth.Token{RefreshToken: siteadapter.BundleForImport(acc.Email, acc.Password, acc.TOTP, acc.Proxy)}); err != nil {
		return err
	}
	// 导入即停用：账号没有可用 token，启用着只会被后台保活当成"待续期"而自动登录
	//（实测：不停用的话 50 个账号会被保活一次性全登，绕过「选中」语义）。
	if a := p.Get(name); a != nil {
		off := false
		a.ApplyAdminPatch(pool.AdminPatch{Enabled: &off})
		p.RefreshAccountEgress(a)
	}
	return nil
}

// loginRequest 组装侧车登录请求。stateDir 留空时不带 storage_state 路径，由侧车按
// PRISM_LOGIN_STATE_DIR（<邮箱>.json）决定——和管理台/保活重登共用同一份设备指纹。
func loginRequest(opt options, acc account, stateDir, name string) siteadapter.LoginRequest {
	req := siteadapter.LoginRequest{
		Email:      acc.Email,
		Password:   acc.Password,
		TOTPSecret: acc.TOTP,
		Proxy:      acc.Proxy,
		Backend:    opt.backend,
		Headless:   !opt.headful,
		Timeout:    240,
	}
	if stateDir != "" {
		req.StorageStateIn = filepath.Join(stateDir, name+".json")
		req.StorageStateOut = req.StorageStateIn
	}
	return req
}

// loginOne 跑一次侧车登录，返回可入库的 token。
func loginOne(ctx context.Context, opt options, acc account, stateDir, name string) (*auth.Token, string) {
	start := time.Now()
	fmt.Printf("→ %s 开始登录（backend=%s headful=%v）\n", acc.Email, opt.backend, opt.headful)
	res, err := siteadapter.RunSidecarLogin(ctx, loginRequest(opt, acc, stateDir, name), func(state, detail string) {
		if detail == "" || detail == state {
			fmt.Printf("    · %s\n", state)
			return
		}
		fmt.Printf("    · %s %s\n", state, truncate(detail, 90))
	})
	if err != nil {
		return nil, fmt.Sprintf("登录失败（%s）: %v", time.Since(start).Round(time.Second), err)
	}
	bundle := &siteadapter.CredentialBundle{
		OAI:       res.OAI,
		Refresh:   res.Refresh,
		Email:     acc.Email,
		Password:  acc.Password,
		TOTP:      acc.TOTP,
		Proxy:     acc.Proxy,
		Backend:   opt.backend,
		Headless:  !opt.headful,
		StatePath: res.StorageState,
	}
	return &auth.Token{
		AccessToken:  res.OAI,
		RefreshToken: bundle.Marshal(),
		APIKey:       res.OAI,
	}, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func openPool(opt options) (*pool.Pool, error) {
	db, err := pg.Open(context.Background(), opt.databaseURL)
	if err != nil {
		return nil, err
	}
	return pool.Open(db, opt.baseURL, opt.baseURL, "", opt.clientType)
}

func storeAccount(p *pool.Pool, name string, token *auth.Token, opt options) error {
	if existing := p.Get(name); existing != nil && !opt.overwrite {
		// 同名已存在：默认覆盖（登录就是要把新凭据写进去），除非显式要求保留。
		log.Printf("账号 %q 已存在，覆盖凭据", name)
	}
	if err := p.SetToken(name, token); err != nil {
		return err
	}
	// 登录成功才启用：导入时是停用状态（等登录），这里放回调度。
	// 与管理台登录任务（Server.setAccountEnabled）保持同一语义。
	if a := p.Get(name); a != nil {
		on := true
		a.ApplyAdminPatch(pool.AdminPatch{Enabled: &on})
		p.RefreshAccountEgress(a)
	}
	return nil
}
