package adapter

import (
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 上游风暴熔断（2026-09-20 线上实锤）：
//
// 上游 start 端点整体挂死时（60s 窗口 223 次 start、0 次进入 poll，全部
// 75s 超时），重试链在挂死上游上空转——每次超时还作废一个健康沙箱并触发
// 8s 重铸风暴，给已经过载的上游再补刀；客户端平均白等 204s、最长 12 分钟。
//
// 状态由 prism 适配器上报（StormRecord：start 阶段成败），api 侧在请求入口
// 与预热巡检读取（StormAllow / StormOpen）。放在 adapter 包而不是 prism，
// 是沿用「pool 不得 import adapter/prism」的既有解耦：api 与 prism 都已
// 依赖本包，不引入新的依赖方向。
//
// 判定（保守，宁晚勿早）：
//   - 采样窗 60s（6×10s 桶环形滚动）；窗内尝试 < stormMinAttempts 不开闸
//     （小流量不判风暴）；
//   - 窗内失败率 ≥ stormFailRate 开闸；
//   - 开闸后 StormAllow 每 stormProbeInterval 放行 1 个探测请求（恢复感知：
//     探测产生的样本会把失败率拉下来，自然闭闸）；
//   - WEB2API_STORM=off 一键关闭（对齐 WEB2API_SENTINEL/KEEPALIVE 开关习惯）。
// ============================================================

const (
	stormWindow        = 60 * time.Second
	stormBuckets       = 6
	stormBucketSpan    = stormWindow / stormBuckets // 10s
	stormMinAttempts   = 12                         // 窗内少于此数不判（正常小流量抖动不算风暴）
	stormFailRate      = 0.7
	stormProbeInterval = 15 * time.Second
)

type stormBucket struct {
	at time.Time // 该桶的时间片起点
	n  int       // 尝试数
	f  int       // 失败数
}

var (
	stormMu        sync.Mutex
	stormRing      [stormBuckets]stormBucket
	stormRingBase  time.Time // buckets[0] 的起始时刻；懒初始化
	stormLastProbe time.Time
)

// stormDisabled 显式关闭（WEB2API_STORM=off）。
func stormDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_STORM")))
	return v == "0" || v == "false" || v == "off"
}

// StormRecord 上报一次 start 阶段成败。4xx 请求过错不上报（与上游健康无关，
// 计入会稀释判定）；只记传输失败、5xx、内联服务端错误与成功。
func StormRecord(ok bool) {
	if stormDisabled() {
		return
	}
	stormMu.Lock()
	defer stormMu.Unlock()
	b := stormCurrentLocked(time.Now())
	b.n++
	if !ok {
		b.f++
	}
}

// stormCurrentLocked 取 now 所在桶，顺带把环形窗口滚动到位（滑出窗口的槽位清零复用）。
func stormCurrentLocked(now time.Time) *stormBucket {
	cur := now.Truncate(stormBucketSpan)
	// 未初始化、时钟回拨、或长眠后整环陈旧：整环重置。
	if stormRingBase.IsZero() || cur.Before(stormRingBase) || cur.Sub(stormRingBase) >= stormWindow+stormBucketSpan {
		stormRingBase = cur
		for i := range stormRing {
			stormRing[i] = stormBucket{at: cur.Add(time.Duration(i) * stormBucketSpan)}
		}
		return &stormRing[0]
	}
	b := &stormRing[int(cur.Sub(stormRingBase)/stormBucketSpan)%stormBuckets]
	if !b.at.Equal(cur) {
		*b = stormBucket{at: cur}
	}
	return b
}

// stormStatsLocked 汇总窗内样本（陈旧桶不计）。
func stormStatsLocked(now time.Time) (attempts, failures int) {
	for i := range stormRing {
		b := &stormRing[i]
		if b.at.IsZero() || now.Sub(b.at) >= stormWindow {
			continue
		}
		attempts += b.n
		failures += b.f
	}
	return attempts, failures
}

// stormOpenLocked 判定（调用方持锁）。
func stormOpenLocked(now time.Time) bool {
	n, f := stormStatsLocked(now)
	return n >= stormMinAttempts && float64(f)/float64(n) >= stormFailRate
}

// StormOpen 上游风暴是否开闸（纯读，不消耗探测名额）。后台任务（预热巡检、
// 沙箱重铸）用它做节流判断。
func StormOpen() bool {
	if stormDisabled() {
		return false
	}
	stormMu.Lock()
	defer stormMu.Unlock()
	return stormOpenLocked(time.Now())
}

// StormAllow 请求是否放行：闭闸放行；开闸时每 stormProbeInterval 放 1 个探测。
// 入口快速失败用它，探测请求照常走完整链路并上报样本。
func StormAllow() bool {
	if !StormOpen() {
		return true
	}
	stormMu.Lock()
	defer stormMu.Unlock()
	if time.Since(stormLastProbe) >= stormProbeInterval {
		stormLastProbe = time.Now()
		return true
	}
	return false
}

// StormStats 观测用：窗内样本与开闸状态（日志/管理端展示）。
func StormStats() (attempts, failures int, open bool) {
	if stormDisabled() {
		return 0, 0, false
	}
	stormMu.Lock()
	n, f := stormStatsLocked(time.Now())
	stormMu.Unlock()
	return n, f, n >= stormMinAttempts && float64(f)/float64(n) >= stormFailRate
}
