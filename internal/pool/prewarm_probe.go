package pool

import (
	"sync"
	"time"
)

// 沙箱预热观测探针（Phase 2 T2.3 warm-set 调度器配套）。
//
// 跨包约束：sandbox 状态（令牌 / 生成时间）活在 adapter/prism 包的 httpClient
// 内，pool 包不得新增对 adapter/prism 的 import（防循环依赖；且本包已被上层
// 广泛依赖）。因此 Account 本体不加探针字段（也避开与 OnAccountEnabled 钩子
// 改动的合并冲突），改用本文件的包级注册表：prewarm 调度器对 Client 类型断言
// 成功后调用 SetSandboxProbe 注入闭包；Snapshot 经 sandboxProbeSnapshot 读取。
// 未注入时默认 (false, 0)。
//
// 注册表以 *Account 为 key：账号替换（重登/重导）产生新对象时旧条目残留，
// 由下次 sweep 的重注入覆盖/废弃，上限为账号更替次数，可接受。
var (
	sandboxProbeMu sync.RWMutex
	sandboxProbes  = make(map[*Account]func() (warm bool, ageSec int64))
)

// SetSandboxProbe 注入/覆盖沙箱状态探针；fn == nil 表示清除。
func (a *Account) SetSandboxProbe(fn func() (warm bool, ageSec int64)) {
	if a == nil {
		return
	}
	sandboxProbeMu.Lock()
	defer sandboxProbeMu.Unlock()
	if fn == nil {
		delete(sandboxProbes, a)
		return
	}
	sandboxProbes[a] = fn
}

// sandboxProbeSnapshot 读探针快照；未注入或探针 panic 时回 (false, 0)。
func (a *Account) sandboxProbeSnapshot() (warm bool, ageSec int64) {
	sandboxProbeMu.RLock()
	fn := sandboxProbes[a]
	sandboxProbeMu.RUnlock()
	if fn == nil {
		return false, 0
	}
	defer func() {
		// 探针只是观测闭包，不得把 panic 传给 Snapshot 调用方。
		_ = recover()
	}()
	return fn()
}

// SandboxWarm 供调度器温热优先（L1）做 O(1) 判定：未注入探针或已过期均为 false。
func (a *Account) SandboxWarm() bool {
	warm, _ := a.sandboxProbeSnapshot()
	return warm
}

// LastUsedAt 返回上次真实请求时间（warm-set 按 MRU 排序用；零值=从未使用）。
func (a *Account) LastUsedAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastUsedAt
}
