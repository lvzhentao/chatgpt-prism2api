package adapter

import (
	"testing"
	"time"
)

// 风暴判定边界：小流量不判、阈值开闸、成功率稀释闭闸、探测限流、开关旁路。
// 直接操作包内状态（同包测试），每个用例前重置环形窗口。
func resetStorm() {
	stormMu.Lock()
	stormRing = [stormBuckets]stormBucket{}
	stormRingBase = time.Time{}
	stormLastProbe = time.Time{}
	stormMu.Unlock()
}

func TestStormOpenNeedsMinAttempts(t *testing.T) {
	resetStorm()
	for range stormMinAttempts - 1 {
		StormRecord(false) // 全失败但样本不足
	}
	if StormOpen() {
		t.Fatalf("样本 %d < %d 不应开闸", stormMinAttempts-1, stormMinAttempts)
	}
}

func TestStormOpensAtFailureRate(t *testing.T) {
	resetStorm()
	// 12 次尝试 9 失败（75% ≥ 70%）→ 开闸
	for range 9 {
		StormRecord(false)
	}
	for range 3 {
		StormRecord(true)
	}
	if !StormOpen() {
		t.Fatal("12 次尝试 75% 失败应开闸")
	}
	n, f, open := StormStats()
	if n != 12 || f != 9 || !open {
		t.Fatalf("stats=%d/%d open=%v want 12/9/true", n, f, open)
	}
}

func TestStormStaysClosedBelowRate(t *testing.T) {
	resetStorm()
	// 12 次尝试 8 失败（66.7% < 70%）→ 不开闸（宁晚勿早）
	for range 8 {
		StormRecord(false)
	}
	for range 4 {
		StormRecord(true)
	}
	if StormOpen() {
		t.Fatal("66.7% 失败率低于阈值不应开闸")
	}
}

func TestStormSuccessDilutesAndCloses(t *testing.T) {
	resetStorm()
	for range 12 {
		StormRecord(false)
	}
	if !StormOpen() {
		t.Fatal("100% 失败应开闸")
	}
	// 恢复期成功涌入：窗内失败率被稀释到阈值下 → 自然闭闸
	for range 14 {
		StormRecord(true)
	}
	if StormOpen() {
		t.Fatal("成功样本稀释后应闭闸")
	}
}

func TestStormAllowProbesAtIntervalWhenOpen(t *testing.T) {
	resetStorm()
	for range 12 {
		StormRecord(false)
	}
	if !StormOpen() {
		t.Fatal("前置：应开闸")
	}
	if !StormAllow() {
		t.Fatal("开闸后首个请求应放行（探测）")
	}
	for range 3 {
		if StormAllow() {
			t.Fatal("探测间隔内不应再放行")
		}
	}
	// 探测间隔过后再放行一个
	stormMu.Lock()
	stormLastProbe = time.Now().Add(-stormProbeInterval)
	stormMu.Unlock()
	if !StormAllow() {
		t.Fatal("探测间隔过后应放行")
	}
}

func TestStormDisabledSwitch(t *testing.T) {
	resetStorm()
	t.Setenv("WEB2API_STORM", "off")
	defer func() { t.Setenv("WEB2API_STORM", "") }()
	for range 20 {
		StormRecord(false)
	}
	if StormOpen() || StormAllow() != true {
		t.Fatal("off 必须旁路：不开闸且全放行")
	}
}

func TestStormWindowExpiresOldBuckets(t *testing.T) {
	resetStorm()
	stormMu.Lock()
	// 往第一个桶塞陈旧样本（80s 前，已滑出 60s 窗口）
	stormRingBase = time.Now().Add(-stormWindow - 20*time.Second).Truncate(stormBucketSpan)
	for i := range stormRing {
		stormRing[i] = stormBucket{at: stormRingBase.Add(time.Duration(i) * stormBucketSpan)}
	}
	stormRing[0].n, stormRing[0].f = 100, 100
	stormMu.Unlock()
	if StormOpen() {
		t.Fatal("窗口外的陈旧样本不应参与判定")
	}
	// 新样本落在当前桶后，陈旧桶仍不计入
	StormRecord(false)
	n, _, _ := StormStats()
	if n != 1 {
		t.Fatalf("窗内样本 %d，应只计新样本 1", n)
	}
}
