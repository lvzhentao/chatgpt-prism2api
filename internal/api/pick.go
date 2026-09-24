package api

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/failclass"
	"prism-2api/internal/pool"
	"prism-2api/internal/scheduler"
)

// accountIter 对应 CPA Manager.Execute 外层等待 + executeMixedOnce 的 tried 集合。
type accountIter struct {
	s       *Server
	ctx     context.Context
	model   string
	headers http.Header
	body    []byte
	tried   map[string]struct{}
	outer   int
	group   string
	// fallbackAffinity 见 scheduler.Request.FallbackAffinity：客户端没给会话标识时用它兜底。
	fallbackAffinity string
}

func (s *Server) newAccountIter(ctx context.Context, r *http.Request, model string, body []byte) *accountIter {
	if ctx == nil && r != nil {
		ctx = r.Context()
	}
	headers := http.Header{}
	if r != nil {
		headers = r.Header
		if ctx == nil {
			ctx = r.Context()
		}
	}
	group, fallbackAffinity := "", ""
	if id, ok := IdentityFrom(ctx); ok {
		group = id.Group
		switch {
		case id.KeyID != 0:
			fallbackAffinity = "key:" + strconv.FormatUint(id.KeyID, 10)
		default:
			// 主 key（KeyID=0）也要能粘：按 UA 分桶 —— 同一类客户端粘同一个账号，
			// 不同客户端仍分散。不带 UA 就没有兜底（走策略轮询），保持原行为。
			if ua := headers.Get("User-Agent"); strings.TrimSpace(ua) != "" {
				h := fnv.New64a()
				_, _ = h.Write([]byte(ua))
				fallbackAffinity = "ua:" + strconv.FormatUint(h.Sum64(), 16)
			}
		}
	}
	return &accountIter{
		s:                s,
		ctx:              ctx,
		model:            model,
		headers:          headers,
		body:             body,
		tried:            map[string]struct{}{},
		group:            group,
		fallbackAffinity: fallbackAffinity,
	}
}

func (it *accountIter) cfg() scheduler.Config {
	if it.s == nil || it.s.sched == nil {
		return scheduler.DefaultConfig()
	}
	return it.s.sched.Config()
}

// Next 选下一张号并占用并发槽位。全冷却且等待 ≤ max-retry-interval 时睡再试（CPA shouldRetryAfterError）。
func (it *accountIter) Next() (acc *pool.Account, err error) {
	// T0.2：记录选号耗时（含冷却等待），供 Phase 0 基线分析。
	t0 := time.Now()
	defer func() {
		name, tried := "", 0
		if it != nil {
			tried = len(it.tried)
		}
		if acc != nil {
			name = acc.Name
		}
		log.Printf("prism: sched_pick account=%q tried=%d latency_ms=%d err=%v", name, tried, time.Since(t0).Milliseconds(), err)
	}()
	if it == nil || it.s == nil {
		return nil, errors.New("scheduler not configured")
	}
	if it.s.sched == nil {
		// 无调度器：直连 pool.Next；满号竞态下重选（最多 len(accounts) 次）。
		for range it.s.pool.Count() + 1 {
			pickAcc, pickErr := it.s.pool.Next()
			if pickErr != nil {
				err = pickErr
				return nil, err
			}
			if pickAcc.Acquire() {
				it.tried[pickAcc.ID] = struct{}{}
				acc = pickAcc
				return acc, nil
			}
		}
		err = errors.New("no available accounts (all at concurrency limit)")
		return nil, err
	}
	cfg := it.cfg()
	for {
		limit := cfg.CredentialLimit()
		if limit > 0 && len(it.tried) >= limit {
			err = errors.New("max-retry-credentials exhausted")
			return nil, err
		}
		var pickAcc *pool.Account
		var pickErr error
		pickAcc, pickErr = it.s.sched.Pick(it.s.pool.Accounts(), scheduler.Request{
			Model:   it.model,
			Headers: it.headers,
			Body:    it.body,
			Tried:   it.tried,
			Group:   it.group,

			FallbackAffinity: it.fallbackAffinity,
		})
		if pickErr == nil {
			if pickAcc.Acquire() {
				it.tried[pickAcc.ID] = struct{}{}
				acc = pickAcc
				return acc, nil
			}
			// 满号竞态：collectReady 已排除满号，此处仅并发抢占；重选不计数。
			continue
		}
		var unavail *scheduler.UnavailableError
		if !errors.As(pickErr, &unavail) {
			err = pickErr
			return nil, err
		}
		if it.outer >= cfg.RequestRetry || cfg.MaxRetryInterval <= 0 {
			err = pickErr
			return nil, err
		}
		wait := unavail.ResetIn
		if wait <= 0 || wait > cfg.MaxRetryInterval {
			err = pickErr
			return nil, err
		}
		if werr := scheduler.WaitForCooldown(it.ctx, wait, cfg.MaxRetryInterval); werr != nil {
			err = werr
			return nil, err
		}
		it.tried = map[string]struct{}{}
		it.outer++
	}
}

// accName 取账号名（nil 安全，供 t_pick 日志用）。
func accName(acc *pool.Account) string {
	if acc == nil {
		return ""
	}
	return acc.Name
}

// accInflight 取账号当时在飞数（T0.2/T5.3 同沙箱并发相关性分析用；nil 安全）。
func accInflight(acc *pool.Account) int {
	if acc == nil {
		return 0
	}
	return acc.Inflight()
}

func (s *Server) noteAccount(acc *pool.Account, err error, model string, clientGone bool, entry *admin.LogEntry, iter *accountIter) failclass.Result {
	res := noteUpstream(acc, err, model, clientGone)
	fillLog(entry, acc, iter, res.Class)
	if res.Cool && s != nil && s.sched != nil && acc != nil {
		s.sched.Invalidate(acc.ID)
	}
	if res.Cool && s != nil && s.logs != nil && acc != nil {
		s.logs.Publish(admin.Event{
			Type:      admin.EventCooldown,
			Time:      time.Now(),
			Account:   acc.Name,
			FailClass: string(res.Class),
		})
	}
	return res
}

func writePickError(w http.ResponseWriter, err error, anthropic bool) {
	var unavail *scheduler.UnavailableError
	if errors.As(err, &unavail) && unavail != nil {
		for k, vs := range unavail.Headers() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		msg := unavail.Error()
		if anthropic {
			writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", msg)
			return
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]string{"message": msg, "type": "rate_limit_error"},
		})
		return
	}
	status := http.StatusServiceUnavailable
	msg := "no available accounts"
	if err != nil {
		msg = err.Error()
	}
	if anthropic {
		writeAnthropicError(w, status, "api_error", msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "upstream_error"},
	})
}

func readJSONBody(r *http.Request, dst any) ([]byte, error) {
	raw, err := io.ReadAll(ioLimit(r.Body))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return raw, err
	}
	return raw, nil
}

func schedulerFromRuntime(rc *admin.RuntimeConfig) scheduler.Config {
	cfg := scheduler.DefaultConfig()
	if rc == nil {
		return cfg
	}
	strategy, affinity, ttl, retry, maxCred, maxWait, disable, warmPreference := rc.SchedulerConfig()
	cfg.Strategy = scheduler.Strategy(strategy)
	cfg.SessionAffinity = affinity
	cfg.SessionAffinityTTL = ttl
	cfg.RequestRetry = retry
	cfg.MaxRetryCredentials = maxCred
	cfg.MaxRetryInterval = maxWait
	cfg.DisableCooling = disable
	cfg.WarmPreference = warmPreference
	return cfg
}
