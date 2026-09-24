package prism

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ============================================================
// 上游 backend 劣化（预热超预算）。
//
// 实测（2026-09-17）：正常时 `/api/backend/1/new` 只要 1.4~2.5s，劣化时变成 **24 ~ 210s**
// 甚至挂死。预热链每跳各有 3 次重试、单跳 75s 上限，不设总量的话一次请求会被拖到
// **3 分钟以上**——对 agent 客户端比直接失败更糟：它不会换号也不会重试，只会一直等。
//
// 所以给整条预热链一个总预算，超了就快速失败。语义对齐 maintenanceError：
// 这是**上游整体不可用**，不是账号问题 ——
//
//	换号 → 没意义（别的账号打的是同一个 backend）；
//	冷却 → 把好账号白关几分钟。
//
// failclass 认 "sandbox degraded" 文案（Unavailable / Cool=false / Switch=false），
// 客户端立刻拿到 503 可以自己重试，而不是握着连接等 3 分钟。
// ============================================================

// warmupBudget 是一次预热链的总预算（生产值；测试里调小）。
// 取值依据：生产实测正常预热 7~35s（含 wait-for-sync 慢到 26s 的一次），
// 劣化时单跳就 24~210s —— 45s 正好切在两者之间。
// 可用 WEB2API_WARMUP_BUDGET_SEC 覆盖（<=0 时回退 45s；热改下次请求生效）。
var warmupBudget = 45 * time.Second

func warmupBudgetOf() time.Duration {
	if v := strings.TrimSpace(os.Getenv("WEB2API_WARMUP_BUDGET_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		log.Printf("prism: WEB2API_WARMUP_BUDGET_SEC=%q 非法，回退 45s", v)
	}
	return warmupBudget
}

// degradedError 表示上游 backend 服务劣化。
type degradedError struct{ Msg string }

func (e *degradedError) Error() string {
	if e.Msg == "" {
		return "prism: upstream sandbox degraded"
	}
	return "prism: upstream sandbox degraded: " + e.Msg
}

// StatusCode 让 failclass 拿到 503。
func (e *degradedError) StatusCode() int { return http.StatusServiceUnavailable }

// isDegraded 判断错误链里是否有劣化标记。
func isDegraded(err error) bool {
	var de *degradedError
	return errors.As(err, &de)
}

// degradedFrom 识别「上游整体不可用」的 5xx。
//
// 真机（2026-09-17 劣化期）：`/auth/session` 由 Cloudflare 直接回 504
// （"Error 504: Gateway time-out / The origin web server…"），`/api/file-management/projects`
// 回 503 —— 都是 edge 到 origin 打不通，换账号打的是同一个 origin，重试只是白等 30s。
// 所以这三种状态码按劣化处理（同维护：不冷却、不换号、直接 503）。
func degradedFrom(status int, body []byte) *degradedError {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &degradedError{Msg: "upstream " + strconv.Itoa(status) + ": " + truncate(string(body), 120)}
	}
	return nil
}

// withWarmupBudget 给预热链套总预算：超时且不是调用方自己取消时，改报劣化。
func (c *httpClient) withWarmupBudget(ctx context.Context, run func(context.Context) (string, error)) (string, error) {
	budgetCtx, cancel := context.WithTimeout(ctx, warmupBudgetOf())
	defer cancel()
	tok, err := run(budgetCtx)
	if err == nil {
		return tok, nil
	}
	if errors.Is(budgetCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		// 预算用完 = 上游这次给不出沙箱，而不是我们请求写错了。
		return "", &degradedError{Msg: err.Error()}
	}
	return tok, err
}
