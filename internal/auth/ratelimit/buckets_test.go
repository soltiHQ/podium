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
