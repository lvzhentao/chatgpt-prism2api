package api

import (
	"context"
	"time"
)

// M2 入口闸门（CAPACITY-PLAN L2）：/v1/* 请求先拿全局并发槽，拿不到排队等待，
// 超时回 429+Retry-After（NewAPI 渠道语义正确），绝不无限挂死。
// 容量 = gateway_concurrency（配置中心，0=自动：池号数 × 单号并发）；
// 在飞请求持旧槽释放（闭包捕获 channel），resize 只影响新请求。

const (
	gateRecheckInterval = 30 * time.Second // 容量重算周期（号数/配置变化后至多 30s 生效）
	gateDefaultQueueSec = 30
	gateRetryAfterSec   = 3
)

func (s *Server) gateCapacity() int {
	if s.runtime == nil {
		return 0
	}
	if n := s.runtime.GatewayConcurrencyN(); n > 0 {
		return n
	}
	conc := s.runtime.AccountConcurrencyN()
	if conc <= 0 {
		conc = 100
	}
	n := s.pool.Count()
	if n <= 0 {
		return 0 // 空池：不设闸，让选号路径给出明确错误
	}
	return n * conc
}

func (s *Server) gateQueueSec() time.Duration {
	sec := gateDefaultQueueSec
	if s.runtime != nil {
		if n := s.runtime.GatewayQueueSecN(); n > 0 {
			sec = n
		}
	}
	return time.Duration(sec) * time.Second
}

// gateAcquire 尝试拿槽：成功返回所属 channel（释放时用），排队超时返回 nil。
func (s *Server) gateAcquire(ctx context.Context) (chan struct{}, bool) {
	s.gateMu.Lock()
	now := time.Now()
	if s.gate == nil || now.Sub(s.gateCheckedAt) > gateRecheckInterval {
		if want := s.gateCapacity(); want > 0 {
			if s.gate == nil || want != cap(s.gate) {
				// 缩容：只搬得动的令牌；扩容：直接换新。在飞请求持旧 ch 释放不受影响。
				next := make(chan struct{}, want)
				if s.gate != nil {
				drain:
					for {
						select {
						case <-s.gate:
							select {
							case next <- struct{}{}:
							default: // 新容量已满：放弃搬运（在飞数会自然回落）
								break drain
							}
						default:
							break drain
						}
					}
				}
				s.gate = next
			}
			s.gateCheckedAt = now
		}
	}
	ch, queue := s.gate, s.gateQueueSec()
	s.gateMu.Unlock()
	if ch == nil {
		return nil, true // 容量未配置/空池：闸门直通
	}
	select {
	case ch <- struct{}{}:
		return ch, true
	case <-time.After(queue):
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}
