package admin

import (
	"testing"
	"time"
)

func TestLogStoreSubscribeAndPublish(t *testing.T) {
	s := NewLogStore(8)
	ch, unsub := s.Subscribe()
	defer unsub()

	s.Add(LogEntry{Path: "/v1/models", Status: 200, Time: time.Now(), FailClass: "rate_limit"})
	select {
	case ev := <-ch:
		if ev.Type != EventLog || ev.Log == nil || ev.Log.Path != "/v1/models" {
			t.Fatalf("log event: %+v", ev)
		}
		if ev.Log.FailClass != "rate_limit" {
			t.Fatalf("fail_class: %s", ev.Log.FailClass)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for log event")
	}

	s.Publish(Event{Type: EventCooldown, Account: "acc1", FailClass: "rate_limit"})
	select {
	case ev := <-ch:
		if ev.Type != EventCooldown || ev.Account != "acc1" {
			t.Fatalf("cooldown event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for cooldown")
	}

	s.Publish(Event{Type: EventLogin, User: "admin"})
	select {
	case ev := <-ch:
		if ev.Type != EventLogin || ev.User != "admin" {
			t.Fatalf("login event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for login")
	}
}

func TestQueryFilterFailClass(t *testing.T) {
	s := NewLogStore(8)
	s.Add(LogEntry{Path: "/v1/messages", Status: 200, Account: "a", Model: "m", FailClass: ""})
	s.Add(LogEntry{Path: "/v1/messages", Status: 503, Account: "a", Model: "m", FailClass: "server"})
	got := s.QueryFilter(10, LogFilter{FailClass: "server"})
	if len(got) != 1 || got[0].Status != 503 {
		t.Fatalf("got %+v", got)
	}
}

func TestComputeStatsAggregates(t *testing.T) {
	s := NewLogStore(8)
	kid := uint64(7)
	s.Add(LogEntry{
		Path: "/v1/chat/completions", Status: 200, Account: "a", Model: "m",
		Prompt: 10, Completion: 5, Total: 15, FailClass: "", ClientKeyID: &kid,
	})
	s.Add(LogEntry{
		Path: "/v1/chat/completions", Status: 429, Account: "a", Model: "m",
		Total: 0, FailClass: "rate_limit", ClientKeyID: &kid,
	})
	st := s.ComputeStats()
	if st.TotalRequests != 2 || st.ErrorRequests != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.ByFailClass["rate_limit"] != 1 {
		t.Fatalf("by_fail_class %+v", st.ByFailClass)
	}
	if st.ByClientKey["7"] != 2 {
		t.Fatalf("by_client_key %+v", st.ByClientKey)
	}
	if st.TokensByAccount["a"] != 15 || st.TokensByModel["m"] != 15 {
		t.Fatalf("tokens %+v %+v", st.TokensByAccount, st.TokensByModel)
	}
}

func TestComputeStatsExcludesClientCanceled(t *testing.T) {
	s := NewLogStore(8)
	s.Add(LogEntry{Path: "/v1/messages", Status: 200})
	s.Add(LogEntry{Path: "/v1/messages", Status: 500, FailClass: "server"})
	s.Add(LogEntry{Path: "/v1/messages", Status: 499, FailClass: "canceled"})
	st := s.ComputeStats()
	if st.TotalRequests != 3 {
		t.Fatalf("total %+v", st)
	}
	// 改动前：status >= 400 即算错，error_requests=2、error_rate=2/3。
	legacyErr, legacyRate := 2, 2.0/3.0
	// 改动后：499/canceled 剔除，error_requests=1、error_rate=1/3。
	if st.ErrorRequests != 1 {
		t.Fatalf("error_requests=%d want 1 (legacy=%d) %+v", st.ErrorRequests, legacyErr, st)
	}
	if want := 1.0 / 3.0; st.ErrorRate != want {
		t.Fatalf("error_rate=%v want %v (legacy=%v)", st.ErrorRate, want, legacyRate)
	}
	// ByFailClass 只是分类展示，canceled 桶保留。
	if st.ByFailClass["canceled"] != 1 {
		t.Fatalf("by_fail_class %+v", st.ByFailClass)
	}
}

func TestComputeStatsCanceledFallbackEitherSide(t *testing.T) {
	s := NewLogStore(8)
	// status 落了 499 但 FailClass 没落：靠 status 条件剔除。
	s.Add(LogEntry{Path: "/v1/messages", Status: 499})
	// FailClass 落了 canceled 但 status 没落成 499：靠 FailClass 条件剔除。
	s.Add(LogEntry{Path: "/v1/messages", Status: 500, FailClass: "canceled"})
	st := s.ComputeStats()
	if st.TotalRequests != 2 || st.ErrorRequests != 0 || st.ErrorRate != 0 {
		t.Fatalf("want 2 total 0 error, got %+v", st)
	}
}

func TestSubscribeSlowConsumerDoesNotBlockAdd(t *testing.T) {
	s := NewLogStore(4)
	ch, unsub := s.Subscribe()
	defer unsub()
	for i := 0; i < 64; i++ {
		s.Add(LogEntry{Path: "/x", Status: 200})
	}
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			if n == 0 {
				t.Fatal("expected some events")
			}
			return
		}
	}
}
