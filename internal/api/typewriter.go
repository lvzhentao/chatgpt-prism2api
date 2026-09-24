package api

import (
	"context"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// 打字机输出：上游 prism 是轮询式整轮返回，正文最后一次性到（无真流式），
// 这里把整段正文按可配置 tps 区间随机取速、切片平滑发出（10 帧/秒），
// 客户端获得打字机效果，NewAPI 等网关的统计速率也落在设定区间附近。
// 速率按 estimateTokens 口径折算（ASCII≈4 字符/token，CJK≈1 rune/token），
// 中英文正文都能贴近设定值。WEB2API_STREAM_TPS_MAX<=0 时关闭（一次性发，旧行为）。

const typewriterFPS = 10

func envFloat(name string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return fallback
	}
	return f
}

// streamTPSRange 返回打字机速率区间（每请求在区间内随机采样一次）。
func streamTPSRange() (float64, float64) {
	lo := envFloat("WEB2API_STREAM_TPS_MIN", 40)
	hi := envFloat("WEB2API_STREAM_TPS_MAX", 70)
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

// streamBudgetSeconds 打字机总时长预算：长内容按预算自动提速，保证滴完不超时。
// 背景：TPS 40-70 对长输出（几千 token 的代码/HTML）要滴 1-2 分钟，叠加上游
// 重试链后总时长撞穿 NewAPI 渠道超时（180s/300s），全变 client_gone（线上
// 2026-09-20 实锤：断开精确发生在 3m0s/5m0s）。普通短回复仍按采样速率走拟人节奏。
func streamBudgetSeconds() float64 {
	return envFloat("WEB2API_STREAM_BUDGET", 75)
}

// retryBudget 换号重试链的总时长预算（秒），<=0 关闭。默认 150s：留出
// 上游生成+打字机预算后仍落在 NewAPI 180s 渠道超时之内，超时返回干净错误。
func retryBudget() time.Duration {
	sec := envFloat("WEB2API_RETRY_BUDGET", 150)
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

// typewrite 把 text 切片逐帧经 emit 发出。min/max 为速率区间；
// hi<=0 关闭（一次发完）；帧间隔 1/fps，ctx 取消即停。
func typewrite(ctx context.Context, emit func(string), text string, lo, hi float64) {
	if text == "" {
		return
	}
	tps := hi
	if hi > lo {
		tps = lo + rand.Float64()*(hi-lo)
	}
	if tps <= 0 {
		emit(text)
		return
	}
	runes := []rune(text)
	toks := estimateTokens(text)
	if toks <= 0 {
		emit(text)
		return
	}
	// 时长预算：采样速率滴不完就提速（只提速不降速，短回复节奏不变）。
	if b := streamBudgetSeconds(); b > 0 {
		if need := float64(toks) / b; tps < need {
			tps = need
		}
	}
	perFrame := float64(len(runes)) * (tps / typewriterFPS) / float64(toks)
	if perFrame < 1 {
		perFrame = 1
	}
	for i := 0; i < len(runes); {
		j := int(math.Min(float64(i)+perFrame, float64(len(runes))))
		emit(string(runes[i:j]))
		i = j
		if i >= len(runes) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second / typewriterFPS):
		}
	}
}
