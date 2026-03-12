package event

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestHub() *Hub {
	return NewHub(zerolog.Nop())
}

func TestHub_NotifyDeliversToSubscribers(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := h.Subscribe(ctx)
	h.Notify("agent_update")

	select {
	case ev := <-ch:
		if ev != "agent_update" {
			t.Fatalf("expected agent_update, got %q", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestHub_NotifyMultipleSubscribers(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1 := h.Subscribe(ctx)
	ch2 := h.Subscribe(ctx)

	h.Notify("spec_update")

	for i, ch := range []<-chan string{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev != "spec_update" {
				t.Fatalf("subscriber %d: expected spec_update, got %q", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out", i)
		}
	}
}

func TestHub_SubscribeUnregistersOnCancel(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := h.Subscribe(ctx)
	cancel()

	time.Sleep(50 * time.Millisecond)

	h.mu.RLock()
	n := len(h.clients)
	h.mu.RUnlock()

	if n != 0 {
		t.Fatalf("expected 0 clients after cancel, got %d", n)
	}
	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed")
	}
}

func TestHub_CloseDisconnectsClients(t *testing.T) {
	h := newTestHub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := h.Subscribe(ctx)
	h.Close()

	if _, ok := <-ch; ok {
		t.Fatal("expected channel to be closed after hub close")
	}
}

func TestHub_NotifyAfterCloseIsNoop(t *testing.T) {
	h := newTestHub()
	h.Close()

	h.Notify("test")
}

func TestHub_CloseIdempotent(t *testing.T) {
	h := newTestHub()
	h.Close()
	h.Close()
}

func TestHub_SubscribeAfterCloseReturnsClosed(t *testing.T) {
	h := newTestHub()
	h.Close()

	ch := h.Subscribe(context.Background())
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel from subscribe after close")
	}
}

func TestHub_RecordAndRecentEvents(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	h.Record(UserCreated, Payload{ID: "u1", Name: "alice"})
	h.Record(SpecCreated, Payload{ID: "s1", Name: "nginx"})

	events := h.RecentEvents(10)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != SpecCreated {
		t.Fatalf("expected newest first (SpecCreated), got %q", events[0].Kind)
	}
	if events[1].Kind != UserCreated {
		t.Fatalf("expected oldest second (UserCreated), got %q", events[1].Kind)
	}
}

func TestHub_RecordIssueKindsGoToBothBuffers(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	h.Record(UserCreated, Payload{ID: "u1"})
	h.Record(AgentDisconnected, Payload{ID: "a1"})
	h.Record(RateLimited, Payload{ID: "u2"})

	events := h.RecentEvents(10)
	issues := h.RecentIssues(10)

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
}

func TestHub_DeleteIssues(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	h.Record(AgentDisconnected, Payload{ID: "a1", Name: "alpha"})
	h.Record(AgentDisconnected, Payload{ID: "a2", Name: "beta"})
	h.Record(AgentDisconnected, Payload{ID: "a1", Name: "alpha"})

	removed := h.DeleteIssues(AgentDisconnected, "a1")
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}

	issues := h.RecentIssues(10)
	if len(issues) != 1 {
		t.Fatalf("expected 1 remaining issue, got %d", len(issues))
	}
	if issues[0].Payload.ID != "a2" {
		t.Fatalf("expected remaining issue to be a2, got %q", issues[0].Payload.ID)
	}
}

func TestHub_NotifyDropsForSlowClient(t *testing.T) {
	h := newTestHub()
	defer h.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := h.Subscribe(ctx)

	for i := range clientBufSize + 5 {
		h.Notify("event_" + string(rune('a'+i)))
	}

	count := 0
	for range clientBufSize {
		select {
		case <-ch:
			count++
		default:
		}
	}
	if count != clientBufSize {
		t.Fatalf("expected %d buffered events, got %d", clientBufSize, count)
	}
}

func TestIsIssueKind(t *testing.T) {
	issues := []string{AgentDisconnected, AgentInactive, AgentDeleted, RateLimited}
	for _, k := range issues {
		if !IsIssueKind(k) {
			t.Errorf("expected %q to be an issue kind", k)
		}
	}

	nonIssues := []string{AgentConnected, UserCreated, SpecCreated, SessionCreated, IssueClosed}
	for _, k := range nonIssues {
		if IsIssueKind(k) {
			t.Errorf("expected %q to NOT be an issue kind", k)
		}
	}
}
