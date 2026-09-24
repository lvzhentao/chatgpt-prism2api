package api

import (
	"sort"
	"sync"
	"time"
)

// 实时速率计量（CAPACITY-PLAN M1）：1s 粒度环形桶，全局 + 按模型。
// 打点全部挂在 access-log 中间件（所有请求的唯一收口），零业务侵入。
// RPM 口径 = /v1/* API 请求；admin/静态不计。TPM = prompt+completion tokens。
const (
	meterBuckets = 180 // 3 分钟 @1s
	meterWindow  = 60  // RPM/TPM 主口径窗口
)

type bucket struct {
	incoming, served, rejected uint64
	promptTok, completionTok   uint64
	retries                    uint64
}

type series struct {
	buckets [meterBuckets]bucket
	headSec int64 // buckets[i] 对应的 unix 秒
}

// advance 滚到 sec（清零途经桶）。须持锁。
func (s *series) advance(sec int64) {
	if s.headSec == 0 {
		s.headSec = sec
	}
	for sec-s.headSec >= meterBuckets {
		s.buckets[s.headSec%meterBuckets] = bucket{}
		s.headSec++
	}
	if sec > s.headSec {
		s.headSec = sec
	}
}

// window 聚合最近 win 秒。须持锁且已 advance 到当前。
func (s *series) window(now int64, win int64) (out bucket) {
	if win > meterBuckets {
		win = meterBuckets
	}
	for i := int64(0); i < win; i++ {
		out = addBucket(out, s.buckets[(now-i)%meterBuckets])
	}
	return out
}

func addBucket(a, b bucket) bucket {
	a.incoming += b.incoming
	a.served += b.served
	a.rejected += b.rejected
	a.promptTok += b.promptTok
	a.completionTok += b.completionTok
	a.retries += b.retries
	return a
}

type meters struct {
	mu       sync.Mutex
	global   series
	byModel  map[string]*series
	inflight int64 // 在飞 /v1 请求（进入-离开）
}

func newMeters() *meters {
	return &meters{byModel: map[string]*series{}}
}

func (m *meters) modelSeries(model string) *series {
	s, ok := m.byModel[model]
	if !ok {
		s = &series{}
		m.byModel[model] = s
	}
	return s
}

// Incoming 请求进入（/v1/* 计数，只计全局——入口时模型未知，模型维度在 Done 归类）。
func (m *meters) Incoming() {
	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight++
	m.global.advance(now)
	m.global.buckets[now%meterBuckets].incoming++
}

// Done 请求结束：status>=400 计 rejected，否则 served；tokens/retries 一并入桶。
// 模型序列的 incoming 在此归类（进出口速率稳态近似）。
func (m *meters) Done(model string, status int, promptTok, completionTok, retries int) {
	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight > 0 {
		m.inflight--
	}
	m.global.advance(now)
	ms := m.modelSeries(model)
	ms.advance(now)
	b := &m.global.buckets[now%meterBuckets]
	bm := &ms.buckets[now%meterBuckets]
	bm.incoming++
	if status >= 400 {
		b.rejected++
		bm.rejected++
	} else {
		b.served++
		bm.served++
	}
	b.promptTok += uint64(promptTok)
	bm.promptTok += uint64(promptTok)
	b.completionTok += uint64(completionTok)
	bm.completionTok += uint64(completionTok)
	b.retries += uint64(retries)
	bm.retries += uint64(retries)
}

type meterStats struct {
	RPM           int64   `json:"rpm"`
	TPM           int64   `json:"tpm"`
	ServedRPM     int64   `json:"served_rpm"`
	RejectedRPM   int64   `json:"rejected_rpm"`
	RetryPerReq   float64 `json:"retry_per_req"`
	PromptTPM     int64   `json:"prompt_tpm"`
	CompletionTPM int64   `json:"completion_tpm"`
}

func statsOf(w bucket) meterStats {
	s := meterStats{
		RPM:           int64(w.incoming),
		TPM:           int64(w.promptTok + w.completionTok),
		ServedRPM:     int64(w.served),
		RejectedRPM:   int64(w.rejected),
		PromptTPM:     int64(w.promptTok),
		CompletionTPM: int64(w.completionTok),
	}
	if w.served+w.rejected > 0 {
		s.RetryPerReq = float64(w.retries) / float64(w.served+w.rejected)
	}
	return s
}

// Snapshot 输出全局 + 按模型（60s 窗口主口径 + 3 分钟趋势序列）。
func (m *meters) Snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	m.global.advance(now)
	models := make([]string, 0, len(m.byModel))
	for k := range m.byModel {
		models = append(models, k)
	}
	sort.Strings(models)
	byModel := map[string]meterStats{}
	for _, k := range models {
		byModel[k] = statsOf(m.byModel[k].window(now, meterWindow))
	}
	// 3 分钟 RPM 趋势（每 10s 一个点，供面板 sparkline）。
	trend := make([]int64, 0, 18)
	for i := 18; i > 0; i-- {
		end := now - int64(i-1)*10
		var b bucket
		for j := int64(0); j < 10; j++ {
			b = addBucket(b, m.global.buckets[(end-j)%meterBuckets])
		}
		trend = append(trend, int64(b.incoming)*6)
	}
	return map[string]any{
		"window_sec": meterWindow,
		"inflight":   m.inflight,
		"global":     statsOf(m.global.window(now, meterWindow)),
		"by_model":   byModel,
		"rpm_trend":  trend,
	}
}
