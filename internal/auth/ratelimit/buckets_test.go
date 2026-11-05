package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// threeAtOnce holds 3 tokens, gets one back per second, drops a key idle for a
// minute and asks to log a key's refusal at most every 10 seconds.
func threeAtOnce() *Buckets {
	return NewBuckets(BucketConfig{Burst: 3, Refill: time.Second, Idle: time.Minute, Notice: 10 * time.Second})
}

// takes makes n takes of key at now and returns how many got a token.
func takes(b *Buckets, now time.Time, key string, n int) (got int) {
	for range n {
		if ok, _ := b.Take(now, key); ok {
			got++
		}
	}
	return got
}

func TestBucketsBurstThenOnePerRefill(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()

	if got := takes(b, t0, "k", 5); got != 3 {
		t.Fatalf("%d of 5 takes at once got a token, want the burst of 3", got)
	}
	if got := takes(b, t0.Add(999*time.Millisecond), "k", 1); got != 0 {
		t.Fatal("a token came back before a full refill interval passed")
	}
	if got := takes(b, t0.Add(time.Second), "k", 3); got != 1 {
		t.Fatalf("%d of 3 takes one interval after the burst got a token, want 1", got)
	}
	// Two more intervals pass: two tokens are back, not the whole burst.
	if got := takes(b, t0.Add(3*time.Second), "k", 5); got != 2 {
		t.Fatalf("%d of 5 takes two intervals later got a token, want 2", got)
	}
}

// Time that is not a whole interval counts toward the next token.
func TestBucketsPartialRefillCarriesOver(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	takes(b, t0, "k", 3)

	if got := takes(b, t0.Add(1500*time.Millisecond), "k", 3); got != 1 {
		t.Fatalf("%d of 3 takes after 1.5 intervals got a token, want 1", got)
	}
	if got := takes(b, t0.Add(1999*time.Millisecond), "k", 1); got != 0 {
		t.Fatal("the second token came back early")
	}
	if got := takes(b, t0.Add(2*time.Second), "k", 3); got != 1 {
		t.Fatalf("%d of 3 takes after 2 intervals got a token, want 1: the half interval was lost", got)
	}
}

// A refused take takes nothing from the bucket and delays no refill.
func TestBucketsRefusalsCostNothing(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	takes(b, t0, "k", 3)

	for i := range 1000 {
		if got := takes(b, t0.Add(time.Duration(i)*time.Millisecond), "k", 1); got != 0 {
			t.Fatalf("take %d got a token before the first refill", i)
		}
	}
	if got := takes(b, t0.Add(time.Second), "k", 1); got != 1 {
		t.Fatal("the refill came late after 1000 refused takes")
	}
}

// A bucket that was idle holds the burst, not what the time would add up to.
func TestBucketsNeverHoldMoreThanBurst(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	takes(b, t0, "k", 3)

	if got := takes(b, t0.Add(50*time.Second), "k", 10); got != 3 {
		t.Fatalf("%d of 10 takes after a long pause got a token, want the burst of 3", got)
	}
}

func TestBucketsKeysAreIndependent(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()

	if got := takes(b, t0, "a", 5); got != 3 {
		t.Fatalf("a: %d of 5 got a token, want 3", got)
	}
	if got := takes(b, t0, "b", 5); got != 3 {
		t.Fatalf("b, with a empty: %d of 5 got a token, want 3", got)
	}
}

// A key without a take for Idle is dropped, so memory follows the keys in use.
func TestBucketsIdleKeysAreDropped(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	takes(b, t0, "idle", 1)
	takes(b, t0.Add(time.Second), "edge", 1)
	takes(b, t0, "used", 1)
	takes(b, t0.Add(30*time.Second), "used", 1)
	if len(b.entries) != 3 {
		t.Fatalf("%d keys tracked, want 3", len(b.entries))
	}

	// "idle" has gone a minute without a take, "edge" a second less.
	takes(b, t0.Add(time.Minute), "new", 1)
	keys := map[string]bool{}
	for key := range b.entries {
		keys[key] = true
	}
	if len(keys) != 3 || !keys["edge"] || !keys["used"] || !keys["new"] || b.order.Len() != 3 {
		t.Fatalf("tracked keys %v (%d in the list), want edge, used and new", keys, b.order.Len())
	}
}

// The first refusal of a key asks to be logged, then none does until Notice
// passed, and every key has its own.
func TestBucketsNoticeAsksOncePerInterval(t *testing.T) {
	b := NewBuckets(BucketConfig{Burst: 1, Refill: time.Hour, Idle: 2 * time.Hour, Notice: time.Minute})
	t0 := time.Now()

	if ok, notice := b.Take(t0, "k"); !ok || notice {
		t.Fatalf("the take that got a token = (%v, %v), want (true, false)", ok, notice)
	}
	b.Take(t0, "other")
	if ok, notice := b.Take(t0, "other"); ok || !notice {
		t.Fatalf("first refusal of another key = (%v, %v), want (false, true)", ok, notice)
	}
	for _, tc := range []struct {
		after  time.Duration
		notice bool
	}{
		{0, true},
		{time.Second, false},
		{59 * time.Second, false},
		{time.Minute, true},
		{90 * time.Second, false},
		{2*time.Minute + 1, true},
	} {
		if ok, notice := b.Take(t0.Add(tc.after), "k"); ok || notice != tc.notice {
			t.Fatalf("refusal after %v = (%v, %v), want (false, %v)", tc.after, ok, notice, tc.notice)
		}
	}
}

// A peek reports what a take would find, and takes nothing.
func TestBucketsPeekDoesNotSpend(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()

	for range 50 {
		if ok, notice := b.Peek(t0, "k"); !ok || notice {
			t.Fatalf("Peek of a whole bucket = (%v, %v), want (true, false)", ok, notice)
		}
	}
	if got := takes(b, t0, "k", 5); got != 3 {
		t.Fatalf("%d of 5 takes after the peeks got a token, want the burst of 3", got)
	}
	if ok, _ := b.Peek(t0, "k"); ok {
		t.Fatal("Peek found a token in an empty bucket")
	}
	if ok, _ := b.Peek(t0.Add(time.Second), "k"); !ok {
		t.Fatal("Peek found no token one interval after the burst")
	}
	if got := takes(b, t0.Add(time.Second), "k", 3); got != 1 {
		t.Fatalf("%d of 3 takes after that peek got a token, want 1: the peek took it", got)
	}
}

// At every instant a peek says what the take that follows it finds.
func TestBucketsPeekAgreesWithTake(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	for _, ms := range []int{0, 0, 0, 0, 0, 200, 900, 1000, 1000, 1999, 2000, 2500, 3500, 3500, 3500, 9000, 9000, 9000, 9000} {
		now := t0.Add(time.Duration(ms) * time.Millisecond)
		peek, _ := b.Peek(now, "k")
		if take, _ := b.Take(now, "k"); peek != take {
			t.Fatalf("at +%d ms Peek found %v and the take %v", ms, peek, take)
		}
	}
}

// A key that was never taken has a full bucket, and a peek does not track it:
// callers that have not authenticated cannot make the set grow.
func TestBucketsPeekOfAnUnknownKeyTracksNothing(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()

	if ok, notice := b.Peek(t0, "stranger"); !ok || notice {
		t.Fatalf("Peek of an unknown key = (%v, %v), want (true, false)", ok, notice)
	}
	if len(b.entries) != 0 || b.order.Len() != 0 {
		t.Fatalf("a peek left %d keys tracked", len(b.entries))
	}
}

// A peek is no use of a key: callers that have not authenticated cannot keep a
// key from being dropped.
func TestBucketsPeekIsNotAUse(t *testing.T) {
	b, t0 := threeAtOnce(), time.Now()
	takes(b, t0, "peeked", 1)
	b.Peek(t0.Add(50*time.Second), "peeked")

	takes(b, t0.Add(61*time.Second), "other", 1)
	if _, tracked := b.entries["peeked"]; tracked {
		t.Fatal("a peek kept an idle key")
	}
}

// A peek that finds the bucket empty asks to be logged when a take would, and the
// interval is the take's: neither asks again until Notice has passed.
func TestBucketsPeekSharesTheNoticeWithTake(t *testing.T) {
	b := NewBuckets(BucketConfig{Burst: 1, Refill: time.Hour, Idle: 2 * time.Hour, Notice: time.Minute})
	t0 := time.Now()
	b.Take(t0, "k")

	for _, tc := range []struct {
		step   string
		after  time.Duration
		take   bool // a take, or a peek
		notice bool
	}{
		{"the first refusal, a peek", 0, false, true},
		{"a take within the interval", 10 * time.Second, true, false},
		{"a peek within the interval", 20 * time.Second, false, false},
		{"a peek when the interval is over", time.Minute, false, true},
		{"a take after that peek", 70 * time.Second, true, false},
		{"a take when the interval is over", 2*time.Minute + time.Second, true, true},
		{"a peek after that take", 2*time.Minute + 30*time.Second, false, false},
	} {
		now := t0.Add(tc.after)
		var ok, notice bool
		if tc.take {
			ok, notice = b.Take(now, "k")
		} else {
			ok, notice = b.Peek(now, "k")
		}
		if ok || notice != tc.notice {
			t.Fatalf("%s = (%v, %v), want (false, %v)", tc.step, ok, notice, tc.notice)
		}
	}
}

// At one instant exactly Burst takes win, whoever asks.
func TestBucketsConcurrentTakes(t *testing.T) {
	const burst, workers = 10, 100
	b := NewBuckets(BucketConfig{Burst: burst, Refill: time.Hour, Idle: 2 * time.Hour, Notice: time.Hour})
	now := time.Now()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins = map[string]int{}
	)
	for i := range workers {
		key := fmt.Sprintf("k%d", i%2)
		wg.Go(func() {
			if ok, _ := b.Take(now, key); ok {
				mu.Lock()
				wins[key]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins["k0"] != burst || wins["k1"] != burst {
		t.Fatalf("takes that won: %v, want %d for each key", wins, burst)
	}
}

func TestNewBucketsRejectsNonsense(t *testing.T) {
	for name, cfg := range map[string]BucketConfig{
		"no burst":  {Refill: time.Second},
		"no refill": {Burst: 1},
		"negative":  {Burst: -1, Refill: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewBuckets accepted it")
				}
			}()
			NewBuckets(cfg)
		})
	}
}

// A dropped bucket must be a full one that has not asked to be logged, so Idle
// is never shorter than the time to fill a bucket, nor than Notice.
func TestNewBucketsRaisesIdle(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  BucketConfig
		want time.Duration
	}{
		{"unset", BucketConfig{Burst: 10, Refill: time.Second}, 10 * time.Second},
		{"shorter than the fill", BucketConfig{Burst: 10, Refill: time.Second, Idle: time.Second}, 10 * time.Second},
		{"shorter than the notice", BucketConfig{Burst: 10, Refill: time.Second, Idle: 20 * time.Second, Notice: time.Minute}, time.Minute},
		{"long enough", BucketConfig{Burst: 10, Refill: time.Second, Idle: time.Hour, Notice: time.Minute}, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewBuckets(tc.cfg).cfg.Idle; got != tc.want {
				t.Fatalf("Idle = %v, want %v", got, tc.want)
			}
		})
	}
}
