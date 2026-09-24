package prism

import (
	"context"
	"log"
	"time"
)

// sandboxRefreshAfter 是预热重铸阈值：令牌年龄超过它就主动重铸。
//
// 为什么必须重铸而不是"命中即返回"：ensureSandbox 命中缓存时直接返回、**不**刷新
// sandboxAt，令牌一到 sandboxIdleTTL（2 分钟）就作废。若预热只在"失效后"重铸，两次
// sweep（间隔 60s + ≤30s 抖动）之间会留下最长约 1 分钟的冷窗口 —— 落在窗口里的请求
// 照样付 6~10s 冷链，预热等于白做。取 TTL 的一半：sweep 一到期就换新令牌，令牌年龄
// 始终 < 60s + 链条耗时，远小于 TTL。
var sandboxRefreshAfter = sandboxIdleTTL / 2

// refreshSandbox 主动重铸沙箱（warm-set 预热专用）。与请求路径的三点区别：
//   - 判定看**年龄**不看有无：年龄 < sandboxRefreshAfter 才跳过；
//   - 独立 flight key（"prewarm-sandbox"）：重铸期间请求路径仍能用旧令牌（TTL 内），
//     不会被预热拖住；并发时两条 flight 各跑各的链，发布都走 publishSandbox 的代际保护，
//     较慢的旧令牌不会覆盖较快的已发布新令牌；
//   - 新令牌在链条跑完时才由 publishSandbox 统一写入，失败不动旧状态。
//
// follower 经 DoChan 等 leader：Do 不感知 context，预算 ctx 过期也拦不住 follower
// 一直等到 leader 跑完；select ctx.Done() 让调用方一到点就立即返回。
//
// 返回 refreshed 表示本次是否真的重铸（供日志与用例判定）。
func (c *httpClient) refreshSandbox(ctx context.Context, projectID string) (refreshed bool, err error) {
	c.mu.Lock()
	age, has := time.Since(c.sandboxAt), c.sandbox != ""
	entryAt := c.sandboxAt
	c.mu.Unlock()
	if has && age < sandboxRefreshAfter {
		return false, nil
	}
	ch := c.flight.DoChan("prewarm-sandbox", func() (any, error) {
		return c.prepareSandboxUnshared(ctx, projectID, entryAt)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return false, r.Err
		}
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Prewarm 把该账号的冷链一次走热：ensureSession→ensureProject（命中即复用）→
// 沙箱**主动重铸**（见 refreshSandbox：年龄 ≥ sandboxRefreshAfter 就换新令牌，
// 而不是"失效后才重铸"，否则两次 sweep 之间会留冷窗口）。
// session/project 走请求路径同一份缓存与 flight；沙箱重铸用独立 flight key，
// 不阻塞请求路径（请求在 TTL 内仍可用旧令牌）。
//
// 失败只返回 error（调用方记日志 + 指数退避）；不打冷却、不计 failclass、
// 不动 RPM/用量计数。成功打一行汇总日志（含 refreshed 标记）。
func (c *httpClient) Prewarm(ctx context.Context) error {
	tAll := time.Now()
	t := time.Now()
	if _, err := c.loadCredential(); err != nil {
		return err
	}
	credMs := time.Since(t).Milliseconds()

	sessionHit := c.sessionValid()
	t = time.Now()
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	sessionMs := time.Since(t).Milliseconds()

	c.mu.Lock()
	projectHit := c.projectID != ""
	c.mu.Unlock()
	t = time.Now()
	projectID, err := c.ensureProject(ctx)
	if err != nil {
		return err
	}
	projectMs := time.Since(t).Milliseconds()

	sandboxHit := c.sandboxToken() != ""
	t = time.Now()
	refreshed, err := c.refreshSandbox(ctx, projectID)
	if err != nil {
		return err
	}
	sandboxMs := time.Since(t).Milliseconds()

	log.Printf("prism: prewarm ok session_hit=%t project_hit=%t sandbox_hit=%t refreshed=%t cred_ms=%d session_ms=%d project_ms=%d sandbox_ms=%d total_ms=%d",
		sessionHit, projectHit, sandboxHit, refreshed, credMs, sessionMs, projectMs, sandboxMs, time.Since(tAll).Milliseconds())
	return nil
}

// SandboxStatus 供 api 侧 warm-set 调度器做观测注入（prewarm_boot.go 的
// sandboxStatusProber 接口）：warm=当前有未过期 sandbox 令牌，ageSec=令牌年龄。
// 只读锁，不触发任何网络。
func (c *httpClient) SandboxStatus() (warm bool, ageSec int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sandbox == "" {
		return false, 0
	}
	if !c.sandboxAt.IsZero() && time.Since(c.sandboxAt) >= sandboxIdleTTL {
		return false, int64(time.Since(c.sandboxAt).Seconds())
	}
	return true, int64(time.Since(c.sandboxAt).Seconds())
}
