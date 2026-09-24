package api

import (
	"context"
	"hash/fnv"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"prism-2api/internal/adapter"
	"prism-2api/internal/pool"
)

// Phase 2 T2.3 warm-set 预热子系统（见 docs/TTFB-PLAN.md）。
//
//   - 集合：Enabled 账号按 lastUsedAt MRU 取前 K（K=prewarm_accounts，默认 16，
//     0=关闭）；冷却中跳过。
//   - 巡检：每 prewarm_interval_sec（默认 60s）一轮；账号上次预热成功超过
//     interval（叠加 30s 抖动偏移）才重预热。
//     注：sandbox TTL 活在 adapter/prism 包内（client.go 的 sandboxIdleTTL），api 侧拿不到，
//     故用本地时钟策略近似"过期前重刷"，见 c.due；60s + ≤30s 抖动远小于 TTL。
//   - 并发 workers=prewarmWorkers（默认 10；warm-set 更大时放大到 min(K, prewarmWorkersMax=24)）；
//     boot 首轮各账号按 hash(name)%30s 抖动 stagger，防 N 账号同龄对齐突发。
//   - 失败指数退避 1m→2m→4m→5m封顶（per-account 内存 map，成功复位）。
//   - 预热失败只记日志：不打冷却、不计 failclass、不动 RPM/用量计数
//     （全程不调 MarkSuccess/MarkFailure/RecordUse/BeginUse）。
//   - 请求路径与预热共享 singleflight（T2.2，prism/auth 包内）：请求到达时
//     预热在飞则直接等结果，不重复冷链。
//
// Server 零侵入：控制器寄存在包级 registry（map[*Server]），不改 server.go
// 的 struct 定义；语义同 keepalive（重复调用 no-op，stopPrewarm 关闭）。
const (
	prewarmWorkers = 10 // 并发下限：warm-set 小的时候不需要更多
	// prewarmWorkersMax 是并发上限：全池预热（K=87）时用 24 路，把一轮 sweep 压进
	// interval（否则 87 账号 × 6~10s 链条 / 10 路 ≈ 54~87s 一轮，令牌到 2 分钟 TTL
	// 都刷不完，冷窗口又回来了）；同时别把上游 backend 打爆（semaphore 就是限流阀）。
	prewarmWorkersMax   = 24
	prewarmMinGap       = 10 * time.Second // 两轮 sweep 的最小间隔（扣掉耗时后的下限，防忙等）
	prewarmJitterWindow = 30 * time.Second
	prewarmTimeout      = 2 * time.Minute

	prewarmBackoffBase = time.Minute
	prewarmBackoffMax  = 5 * time.Minute

	// prewarmHookCooldown：Enabled 钩子触发的即时预热 per-account 防抖冷却。
	prewarmHookCooldown = time.Minute
)

// prewarmer 是预热能力的最小接口：具体实现 Prewarm(ctx) error 在
// adapter/prism 包（prewarm.go，T2.1），此处只做类型断言，编译期不依赖 prism。
type prewarmer interface {
	Prewarm(ctx context.Context) error
}

// sandboxStatusProber 暴露沙箱 warm/age；实现在 prism httpClient 侧。
// 命中则把闭包注入 pool 探针注册表，供 Account.Snapshot 展示。
type sandboxStatusProber interface {
	SandboxStatus() (warm bool, ageSec int64)
}

type prewarmController struct {
	stop    chan struct{}
	running atomic.Bool

	mu          sync.Mutex
	failures    map[string]int
	nextAllowed map[string]time.Time
	lastOK      map[string]time.Time
}

// prewarmRegistry 按 *Server 存放控制器（不改 Server struct，避合并冲突）。
var prewarmRegistry sync.Map // map[*Server]*prewarmController

// startPrewarm 启动 warm-set 预热循环；重复调用 no-op；空池安全。
func (s *Server) startPrewarm() {
	if s == nil {
		return
	}
	if _, ok := prewarmRegistry.Load(s); ok {
		return
	}
	c := &prewarmController{
		stop:        make(chan struct{}),
		failures:    make(map[string]int),
		nextAllowed: make(map[string]time.Time),
		lastOK:      make(map[string]time.Time),
	}
	actual, loaded := prewarmRegistry.LoadOrStore(s, c)
	if loaded {
		return
	}
	c = actual.(*prewarmController)
	if !c.running.CompareAndSwap(false, true) {
		return
	}
	// T2.4：账号变为 Enabled 即触发一次即时预热（回调内防抖：per-account 1min
	// 冷却 + 去重在飞；loadAll 启动加载不触发，由 boot stagger 覆盖）。
	pool.OnAccountEnabled(func(name string) {
		v, ok := prewarmRegistry.Load(s)
		if !ok {
			return
		}
		cc, ok := v.(*prewarmController)
		if !ok || cc == nil || !cc.running.Load() {
			return
		}
		now := time.Now()
		cc.mu.Lock()
		if t, hit := cc.nextAllowed[name]; hit && now.Before(t) {
			cc.mu.Unlock()
			return
		}
		cc.nextAllowed[name] = now.Add(prewarmHookCooldown)
		cc.mu.Unlock()
		go func() {
			accs := s.pool.Accounts()
			for _, a := range accs {
				if a == nil || a.Name != name {
					continue
				}
				if !a.Enabled() {
					return
				}
				if cc.prewarmOne(s, a, time.Now()) {
					log.Printf("prism: prewarm hook warmed account=%s", name)
				}
				return
			}
		}()
	})
	go c.loop(s)
}

// stopPrewarm 关闭预热循环（仿 Keepalive.Stop；重复调用 no-op）。
func (s *Server) stopPrewarm() {
	if s == nil {
		return
	}
	v, ok := prewarmRegistry.LoadAndDelete(s)
	if !ok {
		return
	}
	c, ok := v.(*prewarmController)
	if !ok || c == nil {
		return
	}
	if c.running.CompareAndSwap(true, false) {
		close(c.stop)
	}
}
func (c *prewarmController) loop(s *Server) {
	// boot 首轮 stagger：立即扫一次（各账号按抖动延迟触发），之后按 interval 巡检。
	// interval 每轮重读，管理端热改下个周期生效；prewarm_accounts=0 时本轮空转。
	//
	// sweep 是同步跑的，所以「真实节奏 = interval + sweep 时长」；全池预热遇上上游劣化
	// 时一轮能跑到 75s+，等待期里令牌就过期了（TTL 2 分钟）—— 于是从 interval 里扣掉
	// 上一轮耗时，扣到 prewarmMinGap 为止（下限防忙等）。
	c.sweep(s, true, time.Now())
	for {
		start := time.Now()
		// 风暴开闸时跳过本轮巡检：全池 sweep 是 ~2 账号/s 的固定上游压力
		// （122 号×60s 间隔），上游挂死期间只会火上浇油；风暴闭闸后自然恢复。
		if adapter.StormOpen() {
			log.Printf("prism: prewarm sweep skipped (upstream storm open)")
		} else {
			c.sweep(s, false, start)
		}
		wait := prewarmIntervalOf(s) - time.Since(start)
		if wait < prewarmMinGap {
			wait = prewarmMinGap
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-c.stop:
			t.Stop()
			return
		}
	}
}

func prewarmIntervalOf(s *Server) time.Duration {
	if s == nil || s.runtime == nil {
		return 60 * time.Second
	}
	return time.Duration(s.runtime.PrewarmIntervalSecN()) * time.Second
}

func prewarmKOf(s *Server) int {
	if s == nil || s.runtime == nil {
		return 16
	}
	return s.runtime.PrewarmAccountsN()
}

func (c *prewarmController) sweep(s *Server, boot bool, now time.Time) {
	if s == nil || s.pool == nil {
		return
	}
	t0 := time.Now()
	k := prewarmKOf(s)
	if k <= 0 {
		return
	}
	interval := prewarmIntervalOf(s)
	candidates := selectWarmSet(s.pool.Accounts(), k, now)
	if len(candidates) == 0 {
		return
	}
	// 注入 sandbox 探针：Client 若实现 SandboxStatus，包闭包供 Snapshot 展示。
	// 闭包读的是客户端实时状态，不快照，stale 风险仅为"刷新不及时"。
	for _, a := range candidates {
		if a == nil || a.Client == nil {
			continue
		}
		if p, ok := a.Client.(sandboxStatusProber); ok {
			a.SetSandboxProbe(func() (bool, int64) {
				w, age := p.SandboxStatus()
				return w, age
			})
		}
	}
	// 并发随 warm-set 规模放大（下限 prewarmWorkers，上限 prewarmWorkersMax）：
	// 全池预热时一轮要在 interval 内跑完，否则刷新追不上 2 分钟 TTL。
	workers := prewarmWorkers
	if k > workers {
		workers = min(k, prewarmWorkersMax)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var warmed, skipped atomic.Int64
	for _, a := range candidates {
		a := a
		if a == nil || !c.due(a.Name, interval, now) {
			skipped.Add(1)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if boot {
				// boot stagger：先于 workers 信号量等待，不占 worker 槽位；stop 可中断。
				j := prewarmJitter(a.Name)
				t := time.NewTimer(j)
				select {
				case <-t.C:
				case <-c.stop:
					t.Stop()
					return
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if c.prewarmOne(s, a, time.Now()) {
				warmed.Add(1)
			}
		}()
	}
	wg.Wait()
	log.Printf("prism: prewarm sweep accounts=%d warmed=%d skipped=%d workers=%d took_ms=%d",
		len(candidates), warmed.Load(), skipped.Load(), workers, time.Since(t0).Milliseconds())
}

// due 判断账号本轮是否该预热：退避中跳过；从未成功则到期；否则需超过 interval
// 减去抖动（抖动**提前**而不是延后：既让各账号错开，又保证刷新意图落在 interval
// 到期之前 —— 延后 0~30s 会撞上 2 分钟沙箱 TTL，令牌过期而没刷新）。
// 抖动按 interval/4 封顶：interval < 30s 时原始抖动（最大 30s）比 interval 还大，
// due 会恒真，每 prewarmMinGap（10s）就跑一轮，上游负载直接翻倍。
func (c *prewarmController) due(name string, interval time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.nextAllowed[name]; ok && now.Before(t) {
		return false
	}
	last, ok := c.lastOK[name]
	if !ok {
		return true
	}
	jitter := prewarmJitter(name)
	if cap := interval / 4; cap >= 0 && jitter > cap {
		jitter = cap
	}
	return !now.Before(last.Add(interval).Add(-jitter))
}

func (c *prewarmController) prewarmOne(s *Server, a *pool.Account, now time.Time) bool {
	if s == nil || a == nil || a.Client == nil {
		return false
	}
	pw, ok := a.Client.(prewarmer)
	if !ok {
		return false
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), prewarmTimeout)
	defer cancel()
	// 失败只记日志：不调 MarkFailure/MarkSuccess/RecordUse，不碰冷却与计数。
	if err := pw.Prewarm(ctx); err != nil {
		c.mu.Lock()
		c.failures[a.Name]++
		n := c.failures[a.Name]
		c.nextAllowed[a.Name] = now.Add(prewarmBackoff(n))
		c.mu.Unlock()
		log.Printf("prism: prewarm fail account=%s fails=%d backoff=%s err=%v", a.Name, n, prewarmBackoff(n), err)
		return false
	}
	c.mu.Lock()
	delete(c.failures, a.Name)
	delete(c.nextAllowed, a.Name)
	c.lastOK[a.Name] = now
	c.mu.Unlock()
	log.Printf("prism: prewarm ok account=%s ms=%d", a.Name, time.Since(t0).Milliseconds())
	return true
}

// selectWarmSet 挑 warm-set：Enabled 且未冷却的账号按 lastUsedAt MRU 取前 K。
// lastUsedAt 零值（从未使用）排最后；同值按名称排序保单测确定性。
func selectWarmSet(accs []*pool.Account, k int, now time.Time) []*pool.Account {
	if k <= 0 {
		return nil
	}
	eligible := make([]*pool.Account, 0, len(accs))
	for _, a := range accs {
		if a == nil || !a.Enabled() {
			continue
		}
		if cu := a.CooldownUntil(); !cu.IsZero() && now.Before(cu) {
			continue
		}
		eligible = append(eligible, a)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		ti, tj := eligible[i].LastUsedAt(), eligible[j].LastUsedAt()
		if ti.Equal(tj) {
			return eligible[i].Name < eligible[j].Name
		}
		if ti.IsZero() {
			return false
		}
		if tj.IsZero() {
			return true
		}
		return ti.After(tj)
	})
	if len(eligible) > k {
		eligible = eligible[:k]
	}
	return eligible
}

// prewarmBackoff 失败退避：1m→2m→4m→5m封顶；n<=0 返回 0。
func prewarmBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := prewarmBackoffBase << (failures - 1)
	if d <= 0 || d > prewarmBackoffMax {
		return prewarmBackoffMax
	}
	return d
}

// prewarmJitter 每账号固定偏移：hash(name)%30s，防 N 账号同龄对齐突发。
func prewarmJitter(name string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return time.Duration(h.Sum32()%uint32(prewarmJitterWindow/time.Second)) * time.Second
}
