// Package pool 实现多账号管理与轮询负载均衡。
//
// 每个账号拥有独立的 Cursor 凭据（API Key 或浏览器 OAuth 登录），
// 请求时按 round-robin 选择可用账号；失败分类与冷却见 Account.MarkFailure。
package pool

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/auth"
	"prism-2api/internal/egress"
	"prism-2api/internal/persist"
)

// Pool 是多账号池。
type Pool struct {
	mu       sync.Mutex
	accounts []*Account
	rr       int // round-robin 游标

	backend       persist.Backend
	baseURL       string
	clientVersion string
	clientType    string
	websiteURL    string
	globalProxy   string
	egressFn      func(*Account) egress.Settings
	// concurrencyFn 单号并发上限解析（RuntimeConfig，热改即时生效）；新加入的账号也要接上。
	concurrencyFn func() (base, degraded int)

	// 脏账号集合：请求完成路径（MarkSuccess/MarkFailure/RecordUse 等经由
	// a.save()）只做 O(1) 标记，不同步写 PG；后台 flusher 每 1s 批量 saveLocked 落盘。
	// 崩溃丢 ≤1s 计数/用量/冷却状态（同 doc 含 cooldownUntil/failClass，重启后本该
	// 冷却的账号可能立刻被重用再失败一次；见 docs/TTFB-PLAN.md T3.1，已接受，冷却内存态优先）。
	// dirtyMu 独立于 p.mu：标记路径不抢 p.mu，避免慢 PG 反压 pick/Next。
	dirtyMu   sync.Mutex
	dirty     map[string]*Account
	flushStop chan struct{}
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// ValidName 账号名规则：[a-zA-Z0-9_-]，1–32 字符。
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// SanitizeName 把邮箱或自由文本收成合法账号名；邮箱取 @ 前一段。
func SanitizeName(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		raw = raw[:i]
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// New 用内存 Backend 建池；dir 非空时一次性读入旧 JSON（不回写文件）。
func New(dir, baseURL, websiteURL, clientVersion, clientType string) (*Pool, error) {
	b := persist.NewMemory()
	if err := persist.ImportAccountsDir(b, dir); err != nil {
		return nil, err
	}
	return Open(b, baseURL, websiteURL, clientVersion, clientType)
}

// Open 从 Backend 加载账号池。
func Open(b persist.Backend, baseURL, websiteURL, clientVersion, clientType string) (*Pool, error) {
	if b == nil {
		b = persist.NewMemory()
	}
	p := &Pool{
		backend:       b,
		baseURL:       baseURL,
		websiteURL:    websiteURL,
		clientVersion: clientVersion,
		clientType:    clientType,
		dirty:         make(map[string]*Account),
		flushStop:     make(chan struct{}),
	}
	p.loadAll()
	p.startFlusher()
	return p, nil
}

// SetBaseURL 更新后端地址（如 mock 模式），并同步刷新现有账号。
func (p *Pool) SetBaseURL(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseURL = url
	for _, a := range p.accounts {
		a.Tokens.SetBaseURL(url)
		a.Client.SetBaseURL(url)
	}
}

// BaseURL 当前后端地址（启动值来自 env/flag，可被管理台 PUT 覆盖）。
func (p *Pool) BaseURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baseURL
}

// AddAPIKey 添加（或更新）一个 API Key 账号。
func (p *Pool) AddAPIKey(name, apiKey string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid account name %q (allowed: [a-zA-Z0-9_-], 1-32 chars)", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	tokens := auth.NewTokenManager(p.baseURL, apiKey, p.clientVersion, p.clientType)
	acc := newAccount(name)
	acc.Tokens = tokens
	acc.Client = adapter.NewClient(adapter.ClientConfig{BaseURL: p.baseURL, ClientVersion: p.clientVersion, ClientType: p.clientType, TokenProvider: accessTokenOf(tokens)})
	p.bindPersist(acc)

	for i, a := range p.accounts {
		if a.Name == name {
			acc.copyMetaFrom(a)
			p.applyAccountEgressLocked(acc)
			p.applyConcurrencyLocked(acc)
			p.accounts[i] = acc
			return p.saveLocked(acc)
		}
	}
	p.applyAccountEgressLocked(acc)
	p.applyConcurrencyLocked(acc)
	p.accounts = append(p.accounts, acc)
	return p.saveLocked(acc)
}

// StartBrowserLogin 发起浏览器 OAuth 登录，返回登录 URL；后台轮询成功后账号入池并持久化。
func (p *Pool) StartBrowserLogin(name string) (string, string, error) {
	if !nameRe.MatchString(name) {
		return "", "", fmt.Errorf("invalid account name %q (allowed: [a-zA-Z0-9_-], 1-32 chars)", name)
	}
	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return "", "", err
	}
	uuid := newUUID()
	loginURL := auth.LoginURL(p.websiteURL, challenge, uuid)

	baseURL, clientVersion, clientType := p.baseURL, p.clientVersion, p.clientType
	if clientType == "" {
		clientType = "ide"
	}
	tempID := egress.TempIdentity("login-" + name)
	stub := &Account{Name: name, ID: tempID}
	client := p.HTTPClientFor(stub, 30*time.Second)
	p.mu.Lock()
	var settings egress.Settings
	if p.egressFn != nil {
		settings = p.egressFn(stub)
	}
	p.mu.Unlock()
	go func() {
		for i := 0; i < 150; i++ {
			tok, err := auth.PollResult(baseURL, uuid, verifier, clientVersion, clientType, client)
			if err == nil {
				if perr := p.SetToken(name, tok); perr != nil {
					log.Printf("account %q: store token: %v", name, perr)
					return
				}
				if settings.Kind == egress.KindResin {
					if acc := p.Get(name); acc != nil {
						if herr := egress.InheritLease(settings.ResinURL, settings.ResinPlatform, tempID, acc.StableIdentity()); herr != nil {
							log.Printf("account %q inherit resin lease: %v", name, herr)
						}
						p.applyAccountEgress(acc)
					}
				}
				log.Printf("account %q login success", name)
				return
			}
			if !auth.IsSessionPending(err) {
				log.Printf("account %q login poll error: %v", name, err)
				return
			}
			time.Sleep(pollBackoff(i))
		}
		log.Printf("account %q login poll timeout", name)
	}()
	return loginURL, uuid, nil
}

// SetToken 为账号设置令牌（登录轮询成功后调用）。
func (p *Pool) SetToken(name string, tok *auth.Token) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := p.getLocked(name)
	if acc == nil {
		tokens := auth.NewTokenManager(p.baseURL, "", p.clientVersion, p.clientType)
		acc = newAccount(name)
		acc.Tokens = tokens
		acc.Client = adapter.NewClient(adapter.ClientConfig{BaseURL: p.baseURL, ClientVersion: p.clientVersion, ClientType: p.clientType, TokenProvider: accessTokenOf(tokens)})
		p.applyAccountEgressLocked(acc)
		p.applyConcurrencyLocked(acc)
		p.bindPersist(acc)
		p.accounts = append(p.accounts, acc)
	}
	acc.Tokens.SetToken(tok)
	return p.saveLocked(acc)
}

// Next 按 round-robin 返回下一个可用账号。调度器 Pick 才走 priority / 粘性 / WRR。
func (p *Pool) Next() (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accounts)
	if n == 0 {
		return nil, errors.New("no accounts configured (run /v1/accounts to add one)")
	}
	for i := 0; i < n; i++ {
		p.rr = (p.rr + 1) % n
		a := p.accounts[p.rr]
		if a.available() {
			return a, nil
		}
	}
	return nil, errors.New("no available accounts (all not logged in or temporarily disabled)")
}

// Accounts 返回当前账号切片拷贝（调度器用；元素仍是池内对象）。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// HasAvailable 不推进 round-robin 游标，只看是否有可用号。
func (p *Pool) HasAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.available() {
			return true
		}
	}
	return false
}

// List 返回全部账号状态（按名称排序）。
func (p *Pool) List() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Snapshot, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, a.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Count 返回池中账号总数。
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// Get 按名称取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.getLocked(name)
}

// FindDuplicate 按 Cursor 身份（WorkOS / 邮箱 / access JWT）找已有账号。exceptName 排除自身。
func (p *Pool) FindDuplicate(exceptName, email, workosID, accessToken string) *Account {
	if p == nil {
		return nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	workosID = strings.TrimSpace(workosID)
	accessToken = strings.TrimSpace(accessToken)
	if workosID == "" && accessToken != "" {
		workosID = auth.WorkOSUserID(accessToken)
	}
	if email == "" && workosID == "" && accessToken == "" {
		return nil
	}
	exceptName = strings.TrimSpace(exceptName)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a == nil || (exceptName != "" && a.Name == exceptName) {
			continue
		}
		if workosID != "" {
			if id := a.IdentityWorkOS(); id != "" && id == workosID {
				return a
			}
		}
		if email != "" {
			if e := a.IdentityEmail(); e != "" && e == email {
				return a
			}
		}
		if accessToken != "" && a.CurrentAccessToken() == accessToken {
			return a
		}
	}
	return nil
}

// GetByID 按账号 ID 取号。
func (p *Pool) GetByID(id string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// SetGroups 设置账号分组（不校验注册表；校验见 groups.Store.SetGroups）。
func (p *Pool) SetGroups(name string, groups []string) error {
	acc := p.Get(name)
	if acc == nil {
		return fmt.Errorf("account %q not found", name)
	}
	acc.SetGroups(groups)
	return nil
}

// SetRPM 设置账号 RPM（0 = 不限）。
func (p *Pool) SetRPM(name string, rpm int) error {
	acc := p.Get(name)
	if acc == nil {
		return fmt.Errorf("account %q not found", name)
	}
	acc.SetRPM(rpm)
	return nil
}

// RenameGroup 级联替换所有账号 groups[] 中的组名。
func (p *Pool) RenameGroup(old, new string) {
	for _, a := range p.Accounts() {
		a.RenameGroup(old, new)
	}
}

// Remove 删除账号（含持久化文档）。
func (p *Pool) Remove(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.Name == name {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	// 同步清掉脏标记，否则下一次 Flush 会把已删账号 SaveDoc 写活。
	p.dirtyMu.Lock()
	delete(p.dirty, name)
	p.dirtyMu.Unlock()
	if p.backend != nil {
		return p.backend.DeleteDoc(persist.KindAccount, name)
	}
	return nil
}

func (p *Pool) getLocked(name string) *Account {
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func randRead(b []byte) (int, error) { return rand.Read(b) }

// SetEgressResolver 注入按账号解析出口的函数（代理池 / Resin / 全局正代）。
func (p *Pool) SetEgressResolver(fn func(*Account) egress.Settings) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.egressFn = fn
	p.mu.Unlock()
	p.RefreshEgress()
}

// SetConcurrencyResolver 注入单号并发上限解析（来自 RuntimeConfig），对全部账号生效，
// 且此后新加入的账号（导入 cookie / 新增 key / 登录轮询落库）同样接上。
func (p *Pool) SetConcurrencyResolver(fn func() (base, degraded int)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.concurrencyFn = fn
	accs := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, a := range accs {
		a.SetConcurrencyResolver(fn)
	}
}

// applyConcurrencyLocked 给新账号接上并发上限解析（须持 p.mu）。
func (p *Pool) applyConcurrencyLocked(a *Account) {
	if a != nil && p.concurrencyFn != nil {
		a.SetConcurrencyResolver(p.concurrencyFn)
	}
}

// RefreshEgress 按当前解析器刷新全部账号客户端。
func (p *Pool) RefreshEgress() {
	if p == nil {
		return
	}
	p.mu.Lock()
	accs := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, a := range accs {
		p.applyAccountEgress(a)
	}
}

func (p *Pool) applyAccountEgress(acc *Account) {
	if acc == nil {
		return
	}
	p.mu.Lock()
	fn := p.egressFn
	global := p.globalProxy
	p.mu.Unlock()
	if fn != nil {
		acc.ApplyEgress(fn(acc))
		return
	}
	acc.ApplyClientProxy(global)
}

func (p *Pool) applyAccountEgressLocked(acc *Account) {
	if acc == nil {
		return
	}
	if p.egressFn != nil {
		acc.ApplyEgress(p.egressFn(acc))
		return
	}
	acc.ApplyClientProxy(p.globalProxy)
}

// RefreshAccountEgress 刷新单个账号出口。
func (p *Pool) RefreshAccountEgress(acc *Account) {
	p.applyAccountEgress(acc)
}

// HTTPClientFor 给登录轮询等一次性请求构造出口客户端。
func (p *Pool) HTTPClientFor(acc *Account, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	s := egress.Settings{Kind: egress.KindDirect}
	if acc != nil {
		p.mu.Lock()
		fn := p.egressFn
		global := p.globalProxy
		p.mu.Unlock()
		if fn != nil {
			s = fn(acc)
		} else {
			u := acc.ProxyURL()
			if u == "" {
				u = global
			}
			if u != "" {
				s = egress.Settings{Kind: egress.KindHTTP, HTTPProxyURL: u, Account: acc.StableIdentity()}
			} else {
				s.Account = acc.StableIdentity()
			}
		}
	}
	cli, err := egress.HTTPClient(s, timeout)
	if err != nil {
		return &http.Client{Timeout: timeout}
	}
	return cli
}

// SetGlobalProxy 设置全局上游代理并刷新已有账号客户端。
func (p *Pool) SetGlobalProxy(url string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.globalProxy = url
	p.mu.Unlock()
	p.RefreshEgress()
}

// GlobalProxy 返回全局上游代理。
func (p *Pool) GlobalProxy() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.globalProxy
}

func (p *Pool) bindPersist(a *Account) {
	// T3.1：请求完成路径只做 O(1) 脏标记，不同步写 PG；后台 flusher 每 1s 批量落盘。
	// 令牌刷新同样只标记：刷新不频繁，≤1s 落盘延迟可接受。
	a.Tokens.SetOnRefresh(func(t *auth.Token) { p.markDirty(a) })
	a.setPersist(func() { p.markDirty(a) })
}

// saveLocked 持久化账号令牌与调度元数据（须持 p.mu）。
func (p *Pool) saveLocked(a *Account) error {
	if p.backend == nil {
		return nil
	}
	b, err := a.marshalPersisted()
	if err != nil {
		return err
	}
	return p.backend.SaveDoc(persist.KindAccount, a.Name, b)
}

// markDirty 把账号记为待落盘（O(1)，不碰 PG）。请求完成路径（MarkSuccess /
// MarkFailure / ClearCooldown / RecordUse / RememberUsage / ApplyAdminPatch 等经由
// a.save()）只走这里；冷却决策已在内存立即生效，异步的只是落盘。
// 后台 flusher 每 1s 批量 saveLocked 落盘；崩溃丢 ≤1s 计数/用量/冷却状态
// （同 doc 含 cooldownUntil/failClass，重启后本该冷却的账号可能立刻被重用再失败
// 一次；见 docs/TTFB-PLAN.md T3.1，已接受，冷却内存态优先）。
func (p *Pool) markDirty(a *Account) {
	if p == nil || a == nil || p.backend == nil {
		return
	}
	p.dirtyMu.Lock()
	if p.dirty == nil {
		p.dirty = make(map[string]*Account)
	}
	p.dirty[a.Name] = a
	p.dirtyMu.Unlock()
}

// startFlusher 启动后台 1s 合并落盘（Open 时调用一次）。
func (p *Pool) startFlusher() {
	t := time.NewTicker(time.Second)
	go func(stop <-chan struct{}) {
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.Flush()
			}
		}
	}(p.flushStop)
}

// Flush 同步刷全脏账号（测试「写后立即重开读」用例与退出路径用）。
// 短暂持 p.mu 批量写；请求完成路径不得再同步写 PG。
//
// 退出刷盘说明：Pool / Server（internal/api）/ cmd/server/main.go 目前都没有
// Shutdown/Close 钩子，flusher 是放空跑的后台 goroutine；进程退出前的显式
// Flush() 调用覆盖退出刷盘（集成 owner：在 server 关闭链路上接 p.Flush()）。
func (p *Pool) Flush() {
	if p == nil {
		return
	}
	p.dirtyMu.Lock()
	if len(p.dirty) == 0 {
		p.dirtyMu.Unlock()
		return
	}
	accs := make([]*Account, 0, len(p.dirty))
	for _, a := range p.dirty {
		accs = append(accs, a)
	}
	p.dirty = make(map[string]*Account, len(accs))
	p.dirtyMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range accs {
		_ = p.saveLocked(a)
	}
}

// loadAll 从 Backend 加载全部账号。
func (p *Pool) loadAll() {
	if p.backend == nil {
		return
	}
	docs, err := p.backend.ListDocs(persist.KindAccount)
	if err != nil {
		log.Printf("load accounts: %v", err)
		return
	}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, accName := range names {
		if accName == "groups" || !nameRe.MatchString(accName) {
			continue
		}
		b := docs[accName]
		var rec persistedAccount
		if err := json.Unmarshal(b, &rec); err != nil {
			continue
		}
		// 只有 token 的账号、以及「导入但还没登录」的账号（只有凭据束）都要装进池子：
		// 后者要能在管理台/CLI 里被选中去登录，漏掉就等于导入的账号重启后消失。
		if rec.AccessToken == "" && rec.APIKey == "" && strings.TrimSpace(rec.RefreshToken) == "" {
			continue
		}
		tok := tokenFromPersisted(rec)
		tokens := auth.NewTokenManager(p.baseURL, tok.APIKey, p.clientVersion, p.clientType)
		acc := newAccount(accName)
		acc.Tokens = tokens
		acc.Client = adapter.NewClient(adapter.ClientConfig{BaseURL: p.baseURL, ClientVersion: p.clientVersion, ClientType: p.clientType, TokenProvider: accessTokenOf(tokens)})
		if rec.ClientType != "" {

		}
		applyPersisted(acc, rec)
		tokens.SetToken(&tok)
		p.applyAccountEgressLocked(acc)
		p.applyConcurrencyLocked(acc)
		p.bindPersist(acc)
		p.accounts = append(p.accounts, acc)
		wasEnabled := rec.Enabled == nil || *rec.Enabled
		if acc.enabled != wasEnabled || acc.disableReason != rec.DisableReason {
			_ = p.saveLocked(acc)
		}
		log.Printf("account %q loaded", accName)
	}
}

// accessTokenOf 生成 client 的 token provider。
func accessTokenOf(tm *auth.TokenManager) func() (string, error) {
	return func() (string, error) {
		tok, err := tm.Token()
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := randRead(b); err != nil {
		return fmt.Sprintf("mock-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func pollBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 0; i < attempt && i < 10; i++ {
		d = time.Duration(float64(d) * 1.2)
	}
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}
