package scheduler

import (
	"context"
	"math/rand/v2"
	"time"
)

// CPA conductor_selection.go：并发请求不要在同一恢复时刻一起冲第一张号。
const cooldownWaitJitterCap = 2 * time.Second

// jitteredCooldownWait 移植自 CPA jitteredCooldownWait。
func jitteredCooldownWait(wait, maxWait time.Duration) time.Duration {
	if wait <= 0 {
		return wait
	}
	jitterRange := wait / 4
	if jitterRange > cooldownWaitJitterCap {
		jitterRange = cooldownWaitJitterCap
	}
	if maxWait > 0 && jitterRange > maxWait-wait {
		jitterRange = maxWait - wait
	}
	if jitterRange <= 0 {
		return wait
	}
	return wait + rand.N(jitterRange)
}

// WaitForCooldown 移植自 CPA waitForCooldown：全冷却且等待 ≤ max-retry-interval 时睡再试。
func WaitForCooldown(ctx context.Context, wait, maxWait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(jitteredCooldownWait(wait, maxWait))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
