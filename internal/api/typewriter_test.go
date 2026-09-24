package api

import (
	"context"
	"strings"
	"testing"
)

// 中文 1 rune ≈ 1 token：50 rune @100tps → perFrame=10 → 5 帧（测试时长 ~0.4s）。
func TestTypewriterSlicesCJKByRate(t *testing.T) {
	text := strings.Repeat("字", 50)
	var got []string
	typewrite(context.Background(), func(s string) { got = append(got, s) }, text, 100, 100)
	if strings.Join(got, "") != text {
		t.Fatalf("frames must cover full text, got %d frames", len(got))
	}
	if len(got) != 5 {
		t.Fatalf("want 5 frames (50 runes @100tps/10fps), got %d", len(got))
	}
}

// 英文 4 rune ≈ 1 token：perFrame 按 token 口径折算，400 ascii（100 tok）@100tps → perFrame=40 → 10 帧。
func TestTypewriterSlicesASCIIByTokenRate(t *testing.T) {
	text := strings.Repeat("a", 400)
	var got []string
	typewrite(context.Background(), func(s string) { got = append(got, s) }, text, 100, 100)
	if strings.Join(got, "") != text {
		t.Fatalf("frames must cover full text, got %d frames", len(got))
	}
	if len(got) != 10 {
		t.Fatalf("want 10 frames (100 tokens @100tps/10fps), got %d", len(got))
	}
}

// tps<=0 关闭：整段一帧发完（旧行为）。
func TestTypewriterDisabledEmitsSingleFrame(t *testing.T) {
	var got []string
	typewrite(context.Background(), func(s string) { got = append(got, s) }, "hello world", 0, 0)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("want single full frame, got %v", got)
	}
}

// ctx 取消后第一帧即停，不再继续切片。
func TestTypewriterStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var got []string
	typewrite(ctx, func(s string) { got = append(got, s) }, strings.Repeat("字", 1000), 50, 50)
	if len(got) != 1 {
		t.Fatalf("want 1 frame after cancel, got %d", len(got))
	}
}

func TestStreamTPSRangeNormalize(t *testing.T) {
	t.Setenv("WEB2API_STREAM_TPS_MIN", "70")
	t.Setenv("WEB2API_STREAM_TPS_MAX", "40")
	lo, hi := streamTPSRange()
	if lo != 40 || hi != 70 {
		t.Fatalf("reversed range must swap: %v %v", lo, hi)
	}
}
