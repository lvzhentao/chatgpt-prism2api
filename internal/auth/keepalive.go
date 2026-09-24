package auth

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// 对齐 kiro background_refresh：60s 检查、过期前 15 分钟预刷新、失败只记数。
const (
	defaultCheckInterval  = 60 * time.Second
	defaultRefreshBefore  = 15 * time.Minute
	defaultRefreshWorkers = 10
)

// KeepaliveConfig 后台 Token 保活参数。
type KeepaliveConfig struct {
	Interval      time.Duration
	RefreshBefore time.Duration
	Concurrency   int
	// MaxFailures 连败处置阈值：达到后回调 OnDead（由上层删号），0 = 默认 8。
	// 退避序列 1+2+4+…分钟累计约 3 小时，覆盖代理抖动/上游阵发；仍救不回视为死号。
	MaxFailures int
	// OnDead 死号处置回调（删除等）；连败达阈值时同步调用，panic 由上层兜。
	OnDead func(name string, lastErr error)
}

func defaultKeepaliveConfig(c KeepaliveConfig) KeepaliveConfig {
	if c.Interval <= 0 {
		c.Interval = defaultCheckInterval
	}
	if c.RefreshBefore <= 0 {
		c.RefreshBefore = defaultRefreshBefore
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultRefreshWorkers
	}
	if c.MaxFailures <= 0 {
		c.MaxFailures = 8
	}
	return c
}

// Ref 是保活扫描的一个账号（避免 auth 依赖 pool）。
type Ref struct {
	Name   string
	Tokens *TokenManager
}

type flight struct {
	done chan struct{}
	err  error
}

// Keepalive 定期预刷新即将过期的 access token。
// 失败按连败次数指数退避（上游 auth 故障/重登全队失败时，失败号不再每轮
// 占满侧车并发、挤掉其他号的恢复机会），成功清零；不禁用账号。
type Keepalive struct {
	cfg KeepaliveConfig

	mu       sync.Mutex
	inflight map[string]*flight
	failures map[string]int
	nextTry  map[string]time.Time

	stop    chan struct{}
	running atomic.Bool
}

// NewKeepalive 创建保活器（未 Start 时 Tick 仍可单测）。
func NewKeepalive(cfg KeepaliveConfig) *Keepalive {
	return &Keepalive{
		cfg:      defaultKeepaliveConfig(cfg),
		inflight: map[string]*flight{},
		failures: map[string]int{},
		nextTry:  map[string]time.Time{},
		stop:     make(chan struct{}),
	}
}

// Start 启动 60s 循环；空池安全。重复调用是 no-op。
func (k *Keepalive) Start(list func() []Ref) {
	if k == nil || list == nil {
		return
	}
	if !k.running.CompareAndSwap(false, true) {
		return
	}
	go k.loop(list)
}

// Stop 停止后台循环。
func (k *Keepalive) Stop() {
	if k == nil {
		return
	}
	if k.running.CompareAndSwap(true, false) {
		close(k.stop)
	}
}

func (k *Keepalive) loop(list func() []Ref) {
	ticker := time.NewTicker(k.cfg.Interval)
	defer ticker.Stop()
	k.Tick(list())
	for {
		select {
		case <-ticker.C:
			k.Tick(list())
		case <-k.stop:
			return
		}
	}
}

// Tick 扫描一次：JWT 将在 RefreshBefore 内过期则强制 Refresh。
func (k *Keepalive) Tick(refs []Ref) {
	if k == nil || len(refs) == 0 {
		return
	}
	sem := make(chan struct{}, k.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, ref := range refs {
		if !KeepaliveDue(ref.Tokens, k.cfg.RefreshBefore) {
			continue
		}
		ref := ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			k.refreshOne(ref)
		}()
	}
	wg.Wait()
}

// FailureCount 返回该账号连续刷新失败次数（成功清零）。
func (k *Keepalive) FailureCount(name string) int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.failures[name]
}

// FailureCounts 拷贝失败计数（供 dashboard）。
func (k *Keepalive) FailureCounts() map[string]int {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.failures) == 0 {
		return nil
	}
	out := make(map[string]int, len(k.failures))
	for n, c := range k.failures {
		out[n] = c
	}
	return out
}

func (k *Keepalive) refreshOne(ref Ref) {
	if ref.Tokens == nil {
		return
	}
	name := ref.Name
	k.mu.Lock()
	if t, ok := k.nextTry[name]; ok && time.Now().Before(t) {
		k.mu.Unlock()
		return // 退避窗口内：跳过本轮（inflight 合并对它无意义，直接让位）
	}
	if f, ok := k.inflight[name]; ok {
		k.mu.Unlock()
		<-f.done
		return
	}
	f := &flight{done: make(chan struct{})}
	k.inflight[name] = f
	k.mu.Unlock()

	_, err := ref.Tokens.Refresh()
	k.mu.Lock()
	if err != nil {
		k.failures[name]++
		k.nextTry[name] = time.Now().Add(backoffFor(k.failures[name]))
		log.Printf("keepalive: refresh %q failed (count=%d, next in %s): %v", name, k.failures[name], backoffFor(k.failures[name]), err)
		if k.failures[name] >= k.cfg.MaxFailures && k.cfg.OnDead != nil {
			// 死号处置：连败达阈值（退避累计约 3 小时仍救不回），交上层删除。
			// 先清计数防回调内再触发；回调拿到 lastErr 供档案记录。
			delete(k.failures, name)
			delete(k.nextTry, name)
			log.Printf("keepalive: account %q dead after %d consecutive failures: %v", name, k.cfg.MaxFailures, err)
			k.mu.Unlock()
			func() {
				defer func() { _ = recover() }() // 删除回调不允许把保活循环打死
				k.cfg.OnDead(name, err)
			}()
			f.err = err
			close(f.done)
			k.mu.Lock()
			delete(k.inflight, name)
			k.mu.Unlock()
			return
		}
	} else {
		delete(k.failures, name)
		delete(k.nextTry, name)
		log.Printf("keepalive: refresh %q ok", name)
	}
	k.mu.Unlock()

	f.err = err
	close(f.done)

	k.mu.Lock()
	delete(k.inflight, name)
	k.mu.Unlock()
}

// backoffFor 连败 c 次后的退避时长：1,2,4,8…分钟翻倍，封顶 1 小时。
// 第 1 败只退 1 分钟（与扫描间隔同量级），硬失败号几轮内降到小时级。
func backoffFor(c int) time.Duration {
	if c < 1 {
		c = 1
	}
	d := time.Minute
	for i := 1; i < c && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// KeepaliveDue 是否应预刷新：有 refresh token，且 access JWT 将在 before 内过期（含已过期）。
// 无 refresh token 的 API Key 账号跳过（请求路径仍可用 apiKey 换票）。
func KeepaliveDue(m *TokenManager, before time.Duration) bool {
	if m == nil || !m.HasRefreshToken() {
		return false
	}
	tok := m.Current()
	if tok == nil {
		return false
	}
	if tok.AccessToken == "" {
		return true
	}
	exp := JWTExpiry(tok.AccessToken)
	if exp == 0 {
		return false
	}
	return time.Until(time.Unix(exp, 0)) <= before
}
