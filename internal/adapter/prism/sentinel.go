package prism

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// ============================================================
// sentinel token 产线（对话面风控，2026-09-19 上线日实锤）：
//
// 上游 start（/api/llm/response_with_tools_start）开始强制校验
// openai-sentinel-token header——缺失或复用一律应用层 403
// （"Error while processing conversation (403 Forbidden)"），与 IP/账号/
// 请求形状无关（真机实验矩阵：服务器直连、住宅代理、重放浏览器 header 全 403，
// 唯有真浏览器成功）。token 由 login sidecar 的 sentinel SDK（Node）本地铸造，
// 严格一次性（第二次使用即 403）、跨账号通用（同出口 IP 即可）。
//
// 产线形态：sidecar 的 sentinel_token action（见 sidecar/login/login.py）；
// 这里维持一个小预热池削峰（铸造一个约 2~4s：node 冷启 + 两次网络往返），
// 池空时调用方同步铸一个兜底。WEB2API_SENTINEL=off 一键关闭（回退旧行为）。
// ============================================================

var (
	sentinelPoolOnce sync.Once
	sentinelPool     chan sentinelTok
	sentinelDeviceID string // 固定设备 id：token 不绑账号，服务实例伪装成一台设备

	// mint 失败负缓存：sidecar 故障时若不加闸，attempt/换号重试链会连环打 sidecar，
	// 高并发下把它彻底打爆（线上实测 fd/DNS 耗尽 + 主服务 999% CPU 的重试风暴）。
	mintFailMu sync.Mutex
	mintFailAt time.Time
)

// mintBackoff mint 失败后的静默窗口：期间 take 直接报错不再打 sidecar。
const mintBackoff = 30 * time.Second

// sentinelTok 池内条目：token 严格一次性之外还有短时效（线上实测：池内放置
// 分钟级的 token 发 start 一律 403，现铸的新鲜 token 通过），必须带铸造时间。
type sentinelTok struct {
	val string
	at  time.Time
}

// sentinelTokenFreshTTL token 新鲜窗口：取用时超过即弃（保守值，实测分钟级老化）。
const sentinelTokenFreshTTL = 45 * time.Second // 池内老化实测更早失效（挂死形态），收紧窗口

// sentinelPoolInit 池/设备 id 的一次性初始化（takeSentinelToken 与 ensureChatMaterial 共用）。
func sentinelPoolInit() {
	sentinelPool = make(chan sentinelTok, sentinelPoolSize())
	sentinelDeviceID = uuid4() // 裸 uuid：真机 header 的 id 字段即此形态（cdx1_ 前缀会被上游 400）
	go sentinelRefillLoop()
}

// sentinelEnabled：配置了 sidecar 且未被 WEB2API_SENTINEL=off 显式关闭。
func sentinelEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_SENTINEL")))
	if v == "0" || v == "false" || v == "off" {
		return false
	}
	return SidecarURL() != ""
}

// sentinelPoolSize 池深（默认 4，WEB2API_SENTINEL_POOL 可调；池只削峰不囤货——
// token 会老化，深池反而积压过期票）。
func sentinelPoolSize() int {
	if v := strings.TrimSpace(os.Getenv("WEB2API_SENTINEL_POOL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 32 {
			return n
		}
	}
	return 8
}

// takeSentinelToken 取一个一次性 sentinel token：优先池里的新鲜票，否则现铸。
func takeSentinelToken(ctx context.Context) (string, error) {
	if !sentinelEnabled() {
		return "", fmt.Errorf("prism: sentinel disabled")
	}
	sentinelPoolOnce.Do(sentinelPoolInit)
	// 池内只收新鲜票；过期的直接丢弃继续取/铸。
	for {
		select {
		case tok := <-sentinelPool:
			if time.Since(tok.at) <= sentinelTokenFreshTTL {
				return tok.val, nil
			}
			continue // 过期票，丢弃
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		break
	}
	// 池空（或全是过期票）：限时等补货（后台正在铸），超时自己铸。
	// mint 负缓存窗口内直接报错——sidecar 故障时不随重试链连环打它。
	mintFailMu.Lock()
	failBackoff := time.Since(mintFailAt) < mintBackoff
	mintFailMu.Unlock()
	if !failBackoff {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		select {
		case tok := <-sentinelPool:
			if time.Since(tok.at) <= sentinelTokenFreshTTL {
				cancel()
				return tok.val, nil
			}
		case <-waitCtx.Done():
		}
		cancel()
	}
	tok, err := mintSentinelToken(ctx)
	if err != nil {
		mintFailMu.Lock()
		mintFailAt = time.Now()
		mintFailMu.Unlock()
		return "", err
	}
	return tok, nil
}

// sentinelRefillLoop 后台补池：维持浅池流动新鲜（铸一个歇 2s，不积压），
// 铸失败退避（sidecar 故障时不打爆）。
func sentinelRefillLoop() {
	backoff := time.Duration(0)
	for {
		if len(sentinelPool) >= cap(sentinelPool) {
			time.Sleep(10 * time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		tok, err := mintSentinelToken(ctx)
		cancel()
		if err != nil {
			if backoff < 60*time.Second {
				backoff = 30 * time.Second
			}
			log.Printf("prism: sentinel mint failed (refill backoff %s): %v", backoff, err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		select {
		case sentinelPool <- sentinelTok{val: tok, at: time.Now()}:
		default: // 并发下池被补满：丢弃，避免阻塞
		}
		time.Sleep(500 * time.Millisecond) // 高消耗期连续铸造（45s 新鲜窗内出得去就行）
	}
}

// mintSentinelToken 调 login sidecar 的 sentinel_token action 铸一个 token。
// 响应为 NDJSON 事件流（state/done/error），只关心最后的 done.token。
func mintSentinelToken(ctx context.Context) (string, error) {
	tok, _, err := sidecarAction(ctx, "sentinel_token")
	return tok, err
}

// sidecarAction 调 sidecar 的一个 action，从 NDJSON 事件流里取 done 的 token 与 material。
func sidecarAction(ctx context.Context, action string) (token string, material map[string]any, err error) {
	return sidecarActionWithIndex(ctx, action, 0)
}

// sidecarActionWithIndex 带 state_index 的 sidecar 调用（材料池按槽位轮转账号）。
func sidecarActionWithIndex(ctx context.Context, action string, stateIndex int) (token string, material map[string]any, err error) {
	baseURL := pickSidecar() // mint 分流到 sidecar 池（多容器产能翻倍）
	body, merr := json.Marshal(map[string]any{
		"action":      action,
		"flow":        "prism_inference",
		"page_url":    "https://prism.openai.com/",
		"device_id":   sentinelDeviceID,
		"user_agent":  httpUserAgent,
		"state_index": stateIndex,
	})
	if merr != nil {
		return "", nil, merr
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/login", bytes.NewReader(body))
	if rerr != nil {
		return "", nil, rerr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, derr := loginHTTPClient().Do(req)
	if derr != nil {
		return "", nil, fmt.Errorf("prism: %s sidecar %s: %w", action, baseURL, derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", nil, fmt.Errorf("prism: %s sidecar status %d: %s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	scanner := bufio.NewScanner(resp.Body)
	errMsg := ""
	for scanner.Scan() {
		var ev struct {
			Event    string         `json:"event"`
			Token    string         `json:"token"`
			Material map[string]any `json:"material"`
			Message  string         `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Event {
		case "done":
			token = ev.Token
			if ev.Material != nil {
				material = ev.Material
			}
		case "error":
			errMsg = ev.Message
		}
	}
	if token != "" || material != nil {
		return token, material, nil
	}
	if errMsg != "" {
		return "", nil, fmt.Errorf("prism: %s sidecar error: %s", action, truncate(errMsg, 200))
	}
	return "", nil, fmt.Errorf("prism: %s sidecar produced nothing", action)
}

// ============================================================
// 对话材料包（chat material）：上游 2026-09-19 起把 start 与页面上下文强绑定
// （自建会话+预热沙箱 400 / 假沙箱 token sandbox_reconnecting），唯一被接受的
// body 来自浏览器页面。实验矩阵证明材料可跨会话/跨账号复用（换 conv、换 input、
// 换账号 cookie 都 STARTED）→ 一份材料供全池：低频从 sidecar 刷新，发送仍是
// 网关自己的纯 HTTP 通道（高并发不经过浏览器）。
// ============================================================

// 材料池（N 槽环，WEB2API_MATERIAL_SLOTS 可配）：上游按浏览器身份排队/限流——
// 单材料身份扛全池时 start 被挂死（线上 2026-09-20 10:33 实锤：975 进/968 失败，
// userId 单一）。每槽绑定一个账号的 storage_state（sidecar 按 state_index 轮转
// 产出），请求轮转选用，上游侧自然分散到 N 个身份。
// 容量参考：单身份安全 80 RPM（429 边缘 130）；1000 RPM ≈ 13 槽起步、建议 20+
// （codex 大上下文轮次并发 17/s×60s≈1000，按 40-50 并发/身份再翻一倍保险）。
const chatMaterialTTL = 2 * time.Minute // 材料里 cf_bm/__cflb cookie 是分钟级生命，5min 会被上游挂死

// materialSlots 槽数（默认 5；产线吞吐 = workers/30s，需 ≥ 槽/TTL）。
func materialSlots() int {
	if v := strings.TrimSpace(os.Getenv("WEB2API_MATERIAL_SLOTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 32 {
			return n
		}
	}
	return 5
}

type matSlot struct {
	mu        sync.RWMutex
	idx       int
	metadata  map[string]any
	cookie    string
	at        time.Time
	failAt    time.Time
	failCount int // 连续失败退避：30s×2^n，封顶 10min（死账号不浪费产线产能）
}

var (
	chatMatFlight   singleflight.Group
	chatMatSlotArr  []*matSlot
	chatMatSlotOnce sync.Once
	chatMatRR       atomic.Uint64 // 轮转计数
)

func chatMatSlotInit() {
	n := materialSlots()
	chatMatSlotArr = make([]*matSlot, n)
	for i := range chatMatSlotArr {
		chatMatSlotArr[i] = &matSlot{idx: i}
	}
	go chatMatWarmer()
}

func (s *matSlot) fresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata != nil && time.Since(s.at) <= chatMaterialTTL
}

func (s *matSlot) get() (map[string]any, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.metadata == nil || time.Since(s.at) > chatMaterialTTL {
		return nil, "", false
	}
	return s.metadata, s.cookie, true
}

func (s *matSlot) invalidate() {
	s.mu.Lock()
	s.metadata = nil
	s.cookie = ""
	s.mu.Unlock()
}

// ensureChatMaterialSlot 槽位材料缺失/过期时同步刷新（单飞合并；失败 30s 负缓存）。
func ensureChatMaterialSlot(ctx context.Context, s *matSlot) {
	if s.fresh() {
		return
	}
	s.mu.RLock()
	backoff := 30 * time.Second << min(s.failCount, 5)
	if backoff > 10*time.Minute {
		backoff = 10 * time.Minute
	}
	failing := time.Since(s.failAt) < backoff
	s.mu.RUnlock()
	if failing || !sentinelEnabled() {
		return
	}
	chatMatSlotOnce.Do(chatMatSlotInit)
	_, _, _ = chatMatFlight.Do(fmt.Sprintf("material-%d", s.idx), func() (any, error) {
		if s.fresh() {
			return nil, nil
		}
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		_, material, merr := sidecarActionWithIndex(mctx, "chat_material", s.idx)
		if merr != nil {
			s.mu.Lock()
			s.failAt = time.Now()
			s.failCount++
			s.mu.Unlock()
			log.Printf("prism: chat material[%d] refresh failed: %v", s.idx, merr)
			return nil, merr
		}
		meta, _ := material["metadata"].(map[string]any)
		ck, _ := material["cookie"].(string)
		if meta == nil || strAt(meta, "sandbox_token") == "" || strings.TrimSpace(ck) == "" {
			s.mu.Lock()
			s.failAt = time.Now()
			s.mu.Unlock()
			return nil, fmt.Errorf("prism: chat material[%d] incomplete", s.idx)
		}
		s.mu.Lock()
		s.metadata = meta
		s.cookie = ck
		s.at = time.Now()
		s.failCount = 0
		s.mu.Unlock()
		log.Printf("prism: chat material[%d] refreshed (userId=%s)", s.idx, truncate(strAt(meta, "userId"), 24))
		return nil, nil
	})
}

// pickChatMaterial 轮转取一个可用槽位：优先新鲜槽；全过期则现场保温最旧的。
// 返回 metadata、cookie 与槽位指针（400 时按槽作废）。
func pickChatMaterial(ctx context.Context) (map[string]any, string, *matSlot) {
	if !sentinelEnabled() {
		return nil, "", nil
	}
	chatMatSlotOnce.Do(chatMatSlotInit)
	n := uint64(len(chatMatSlotArr))
	start := chatMatRR.Add(1)
	// 第一圈：取现成新鲜的
	for i := uint64(0); i < n; i++ {
		s := chatMatSlotArr[(start+i)%n]
		if meta, ck, ok := s.get(); ok {
			return meta, ck, s
		}
	}
	// 全过期：同步保温 RR 槽（sidecar 串行出料），拿到就用
	s := chatMatSlotArr[start%n]
	ensureChatMaterialSlot(ctx, s)
	if meta, ck, ok := s.get(); ok {
		return meta, ck, s
	}
	// 保温失败：试其他槽的负缓存窗口外现场保温
	for i := uint64(1); i < n; i++ {
		s2 := chatMatSlotArr[(start+i)%n]
		ensureChatMaterialSlot(ctx, s2)
		if meta, ck, ok := s2.get(); ok {
			return meta, ck, s2
		}
	}
	return nil, "", nil
}

// chatMatWarmer 后台保温：每 20s 扫一遍，把所有过期槽并发刷新（≤4 路，与
// sidecar worker 数对齐；TTL 2min × N 槽的产线负载由 worker 池并行消化）。
func chatMatWarmer() {
	for {
		time.Sleep(20 * time.Second)
		if !sentinelEnabled() {
			continue
		}
		var stale []*matSlot
		for _, s := range chatMatSlotArr {
			if !s.fresh() {
				stale = append(stale, s)
			}
		}
		if len(stale) == 0 {
			continue
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, materialWorkerConc()) // 与 sidecar worker 数同源对齐
		for _, s := range stale {
			wg.Add(1)
			sem <- struct{}{}
			go func(s *matSlot) {
				defer wg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				ensureChatMaterialSlot(ctx, s)
			}(s)
		}
		wg.Wait()
	}
}

// materialWorkerConc 材料保温并发：与 sidecar 的 _material_workers_count 读同一
// 环境变量 PRISM_MATERIAL_WORKERS（compose 两侧同名传递），默认/上限一致（4/8）。
// 曾经硬编码 6 而 sidecar 实际默认 4——compose 没把变量传进侧车，两边各说各话。
func materialWorkerConc() int {
	if v := strings.TrimSpace(os.Getenv("PRISM_MATERIAL_WORKERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8 {
			return n
		}
	}
	return 4
}

// currentMaterialCookie 取任一新鲜槽的 cookie（沙箱面请求共用浏览器身份）。
func currentMaterialCookie() string {
	if !sentinelEnabled() {
		return ""
	}
	chatMatSlotOnce.Do(chatMatSlotInit)
	for _, s := range chatMatSlotArr {
		if meta, ck, ok := s.get(); ok {
			_ = meta
			return ck
		}
	}
	return ""
}

// strAt 从 map[string]any 取字符串（材料字段读取用）。
func strAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
