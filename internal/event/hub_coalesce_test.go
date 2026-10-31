package event

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// coalesceWindow is the coalescing window of these tests. Each runs in a
// synctest bubble, where time moves only when every goroutine waits: the
// timings below are exact, and a test takes no wall-clock time.
const coalesceWindow = time.Second

// delivery is an event that reached a client, and when.
type delivery struct {
	at    time.Time
	event string
}

// recorded collects what a client receives until its channel closes.
type recorded struct {
	mu  sync.Mutex
	got []delivery
}

func recordFrom(ch <-chan string) *recorded {
	r := &recorded{}
	go func() {
		for ev := range ch {
			r.mu.Lock()
			r.got = append(r.got, delivery{at: time.Now(), event: ev})
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *recorded) all() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// count returns how many of the events were of the kind.
func (r *recorded) count(kind string) (n int) {
	for _, d := range r.all() {
		if d.event == kind {
			n++
		}
	}
	return n
}

// pendingTimers counts the trailing deliveries that wait on a timer.
func pendingTimers(h *Hub) (n int) {
	h.coalesce.mu.Lock()
	defer h.coalesce.mu.Unlock()
	for _, p := range h.coalesce.pulses {
		if p.timer != nil {
			n++
		}
	}
	return n
}

// 100 refresh pulses in 100 ms reach a client as the first one at once and one
// trailing delivery at the end of the window. A client that fetches on every
// pulse fetches twice, and the second fetch sees the last change.
func TestHubCoalescesRefreshBursts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := recordFrom(h.Subscribe(ctx))

		start := time.Now()
		for range 100 {
			h.Notify(RefreshAgents)
			time.Sleep(time.Millisecond)
		}
		synctest.Wait()
		if d := got.all(); len(d) != 1 || !d[0].at.Equal(start) {
			t.Fatalf("after the burst the client has %d deliveries, want 1: the first pulse, delivered at once", len(d))
		}

		// Pulses arrived after the first delivery, so one more must follow, at
		// the end of the window that the first one started.
		time.Sleep(coalesceWindow)
		synctest.Wait()
		d := got.all()
		if len(d) != 2 {
			t.Fatalf("the burst made %d deliveries, want 2: the first, and one trailing", len(d))
		}
		if want := start.Add(coalesceWindow); !d[1].at.Equal(want) {
			t.Fatalf("trailing delivery at +%v, want +%v: the end of the window", d[1].at.Sub(start), want.Sub(start))
		}

		// The burst is over, and so are the deliveries.
		time.Sleep(10 * coalesceWindow)
		synctest.Wait()
		if n := len(got.all()); n != 2 {
			t.Fatalf("%d deliveries after the burst ended, want still 2", n)
		}
	})
}

// A pulse after a quiet period goes out at once, and a lone pulse has no
// trailing delivery: the window only ever holds back pulses that follow another.
func TestHubDeliversARefreshAfterQuietAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := recordFrom(h.Subscribe(ctx))

		for i := range 3 {
			time.Sleep(3 * coalesceWindow)
			at := time.Now()
			h.Notify(RefreshSpecs)
			synctest.Wait()
			if d := got.all(); len(d) != i+1 || !d[i].at.Equal(at) {
				t.Fatalf("pulse %d after a quiet period: the client has %d deliveries, want %d, the last delivered at once", i+1, len(d), i+1)
			}
		}
		time.Sleep(10 * coalesceWindow)
		synctest.Wait()
		if n := len(got.all()); n != 3 {
			t.Fatalf("%d deliveries for 3 lone pulses, want 3", n)
		}
	})
}

// Under steady load the clients get one pulse per window, never two within a
// window, and the last change of the load is delivered, not dropped.
func TestHubDeliversAtMostOneRefreshPerWindowAndTheLastChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := recordFrom(h.Subscribe(ctx))

		// 10 s of load: a pulse every 100 ms.
		var lastPulse time.Time
		for range 100 {
			h.Notify(RefreshAgents)
			lastPulse = time.Now()
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(2 * coalesceWindow)
		synctest.Wait()

		d := got.all()
		if len(d) < 10 || len(d) > 11 {
			t.Fatalf("%d deliveries for 10 s of load at a %v window, want 10 or 11", len(d), coalesceWindow)
		}
		for i := 1; i < len(d); i++ {
			if gap := d[i].at.Sub(d[i-1].at); gap < coalesceWindow {
				t.Errorf("deliveries %d and %d are %v apart, want at least %v", i, i+1, gap, coalesceWindow)
			}
		}
		if last := d[len(d)-1].at; last.Before(lastPulse) {
			t.Fatalf("the last delivery at %v precedes the last pulse at %v: the last change was lost", last, lastPulse)
		}
	})
}

// Each refresh kind has its own window, and an event that is no refresh pulse
// is never held back.
func TestHubCoalescesEachKindOnItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := recordFrom(h.Subscribe(ctx))

		for range 10 {
			h.Notify(RefreshAgents)
		}
		for range 2 {
			h.Notify(RefreshSpecs)
		}
		for range 3 {
			h.Notify("custom")
		}
		synctest.Wait()
		for kind, want := range map[string]int{RefreshAgents: 1, RefreshSpecs: 1, "custom": 3} {
			if n := got.count(kind); n != want {
				t.Errorf("%s: %d deliveries at once, want %d", kind, n, want)
			}
		}

		time.Sleep(coalesceWindow)
		synctest.Wait()
		for kind, want := range map[string]int{RefreshAgents: 2, RefreshSpecs: 2, "custom": 3} {
			if n := got.count(kind); n != want {
				t.Errorf("%s: %d deliveries after the window, want %d", kind, n, want)
			}
		}
	})
}

// loopback is a Writer that replicates like Raft: every command it gets is
// applied to the hub, once, as the state machine of each replica does.
type loopback struct {
	hub      *Hub
	mu       sync.Mutex
	notifies int
}

func (l *loopback) Notify(ev string) {
	l.mu.Lock()
	l.notifies++
	l.mu.Unlock()
	l.hub.ApplyLocalNotify(ev)
}

func (l *loopback) Record(kind string, p Payload) { l.hub.ApplyLocalRecord(kind, p) }

func (l *loopback) DeleteIssues(kind, id string) int { return l.hub.ApplyLocalDeleteIssues(kind, id) }

// The window applies where a replicated pulse reaches the clients, not where it
// is sent. Every replica applies the pulses of the log to its own hub, so each
// bounds its own clients whichever node sent them, and the trailing delivery
// comes from the pulses that replica applied. What goes into the log is not
// held back: that is the cost of coalescing at the fan-out.
func TestHubCoalescesNotificationsThatCameThroughTheWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		w := &loopback{hub: h}
		h.SetWriter(w)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := recordFrom(h.Subscribe(ctx))

		for range 100 {
			h.Notify(RefreshAgents)
			time.Sleep(time.Millisecond)
		}
		time.Sleep(coalesceWindow)
		synctest.Wait()

		if w.notifies != 100 {
			t.Errorf("the writer replicated %d pulses, want all 100", w.notifies)
		}
		if n := len(got.all()); n != 2 {
			t.Errorf("the client got %d deliveries of 100 replicated pulses, want 2", n)
		}
	})
}

// Close stops the trailing delivery that waits on its timer, and no pulse after
// it schedules another. The bubble checks the goroutines: it ends only when
// every goroutine started in it has returned, and fails if one stays blocked.
func TestHubCloseStopsThePendingTrailingDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		ctx, cancel := context.WithCancel(context.Background())
		got := recordFrom(h.Subscribe(ctx))

		h.Notify(RefreshAgents)
		h.Notify(RefreshAgents) // waits for the end of the window
		h.Notify(RefreshSpecs)
		h.Notify(RefreshSpecs)
		if n := pendingTimers(h); n != 2 {
			t.Fatalf("%d trailing deliveries wait before Close, want 2", n)
		}

		h.Close()
		cancel()
		if n := pendingTimers(h); n != 0 {
			t.Fatalf("%d timers still pending after Close", n)
		}

		h.Notify(RefreshAgents)
		h.Notify(RefreshUsers)
		h.Notify(RefreshUsers)
		if n := pendingTimers(h); n != 0 {
			t.Fatalf("a pulse after Close scheduled %d timers", n)
		}
		time.Sleep(10 * coalesceWindow)
		synctest.Wait()
		if n := len(got.all()); n != 2 {
			t.Fatalf("%d deliveries, want only the two that came before Close", n)
		}
	})
}

// The window never makes a caller wait: a trailing delivery runs on a timer. In
// a bubble the clock moves only while every goroutine waits, so a Notify that
// waited for the end of the window would show as elapsed time.
func TestHubNotifyDoesNotBlockTheCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHub(WithCoalesce(coalesceWindow))
		defer h.Close()
		start := time.Now()
		for range 1000 {
			h.Notify(RefreshAgents)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("1000 pulses took %v, want no waiting", elapsed)
		}
	})
}

// A hub without a window delivers every pulse, as it always did.
func TestHubWithoutWindowDeliversEveryNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"no option", nil},
		{"zero", []Option{WithCoalesce(0)}},
		{"negative", []Option{WithCoalesce(-time.Second)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub(tc.opts...)
			defer h.Close()
			ch := h.Subscribe(t.Context())
			for range 10 {
				h.Notify(RefreshAgents)
			}
			if n := len(ch); n != 10 {
				t.Fatalf("%d of 10 pulses delivered at once, want all", n)
			}
		})
	}
}
