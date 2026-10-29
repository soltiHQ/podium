package ratelimit

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth"
)

func TestNew_DefaultConfig(t *testing.T) {
	l := New(Config{})

	if l.cfg.MaxAttempts != 5 {
		t.Fatalf("expected default MaxAttempts=5, got %d", l.cfg.MaxAttempts)
	}
	if l.cfg.Window != 10*time.Minute {
		t.Fatalf("expected default Window=10m, got %v", l.cfg.Window)
	}
	if l.cfg.BlockWindow != 10*time.Minute {
		t.Fatalf("expected default BlockWindow=10m, got %v", l.cfg.BlockWindow)
	}
}

func TestLimiter_NotBlockedInitially(t *testing.T) {
	l := New(Config{MaxAttempts: 3, BlockWindow: time.Minute})

	if l.blocked("k", time.Now()) {
		t.Fatal("expected not blocked initially")
	}
}

func TestLimiter_BlockAfterThreshold(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 3, BlockWindow: time.Minute})

	l.recordFailure("k", now)
	l.recordFailure("k", now)
	if l.blocked("k", now) {
		t.Fatal("should not be blocked before threshold")
	}

	l.recordFailure("k", now)
	if !l.blocked("k", now) {
		t.Fatal("expected blocked after reaching threshold")
	}
}

func TestLimiter_BlockExpires(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.recordFailure("k", start)

	if !l.blocked("k", start) {
		t.Fatal("expected blocked immediately")
	}

	after := start.Add(2 * time.Minute)

	if l.blocked("k", after) {
		t.Fatal("expected block to expire")
	}

	// state should be cleared after expiry
	if _, ok := l.entries["k"]; ok {
		t.Fatal("expected entry to be removed after expiry")
	}
}

func TestLimiter_Reset(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.recordFailure("k", now)

	if !l.blocked("k", now) {
		t.Fatal("expected blocked")
	}

	l.Reset("k")

	if l.blocked("k", now) {
		t.Fatal("expected not blocked after reset")
	}
}

func TestLimiter_IndependentKeys(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.recordFailure("k1", now)

	if !l.blocked("k1", now) {
		t.Fatal("k1 should be blocked")
	}
	if l.blocked("k2", now) {
		t.Fatal("k2 should not be blocked")
	}
}

func TestLimiter_FailuresAccumulateUntilReset(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 3, BlockWindow: time.Minute})

	l.recordFailure("k", now)
	l.recordFailure("k", now)

	if l.blocked("k", now) {
		t.Fatal("should not be blocked yet")
	}

	l.recordFailure("k", now)

	if !l.blocked("k", now) {
		t.Fatal("expected blocked at exactly threshold")
	}
}

func TestLimiter_ExpiryResetsFailureState(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.recordFailure("k", start)

	after := start.Add(2 * time.Minute)
	if l.blocked("k", after) {
		t.Fatal("block should have expired")
	}

	l.recordFailure("k", after)
	if !l.blocked("k", after) {
		t.Fatal("should block again after new failure")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	const limit = 100_000
	now := time.Now()
	l := New(Config{MaxAttempts: 5, BlockWindow: time.Hour})

	// The oldest key has an attempt in flight, which is never evicted.
	held, err := l.Reserve(now, "held")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	l.recordFailure("first", now)
	l.recordFailure("second", now)
	for i := range limit - 3 {
		l.recordFailure("filler-"+strconv.Itoa(i), now)
	}
	l.recordFailure("first", now) // "second" is now the least recently updated idle key
	l.recordFailure("overflow", now)

	if n := len(l.entries); n > limit {
		t.Fatalf("limiter holds %d keys, want at most %d", n, limit)
	}
	if _, ok := l.entries["second"]; ok {
		t.Fatal("the least recently updated idle key was not evicted")
	}
	for _, key := range []string{"held", "first", "overflow"} {
		if _, ok := l.entries[key]; !ok {
			t.Fatalf("key %q was evicted", key)
		}
	}
}

func TestRecordFailureReportsTheBlockOnce(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 3, Window: time.Minute, BlockWindow: time.Minute})

	for _, at := range []time.Time{start, start.Add(2 * time.Minute)} {
		for i, want := range []bool{false, false, true, false} {
			if got := l.recordFailure("k", at); got != want {
				t.Fatalf("failure %d at %v: recordFailure = %v, want %v", i+1, at.Sub(start), got, want)
			}
		}
		if !l.blocked("k", at) {
			t.Fatalf("not blocked at %v", at.Sub(start))
		}
	}
}

func TestFailuresCountWithinWindowOfTheLastOne(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 3, Window: time.Minute, BlockWindow: time.Hour})

	// Each failure lands within a minute of the previous one.
	l.recordFailure("k", start)
	l.recordFailure("k", start.Add(50*time.Second))
	l.recordFailure("k", start.Add(100*time.Second))
	if !l.blocked("k", start.Add(100*time.Second)) {
		t.Fatal("failures within the window of each other did not block")
	}
	if !l.blocked("k", start.Add(100*time.Second+59*time.Minute)) {
		t.Fatal("block ended before BlockWindow")
	}
	if l.blocked("k", start.Add(100*time.Second+time.Hour)) {
		t.Fatal("block outlived BlockWindow")
	}

	// Failures further apart than the window never add up.
	for i := range 5 {
		l.recordFailure("spaced", start.Add(time.Duration(i)*61*time.Second))
	}
	if l.blocked("spaced", start.Add(5*61*time.Second)) {
		t.Fatal("failures more than a window apart added up")
	}
	if l.blocked("spaced", start.Add(10*time.Minute)) {
		t.Fatal("unblocked key reported blocked after its window")
	}
	if _, ok := l.entries["spaced"]; ok {
		t.Fatal("expected the entry to be removed a window after its last failure")
	}
}

func TestReserveCountsAttemptsInFlight(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 3, Window: time.Minute, BlockWindow: time.Minute})

	var held []*Attempt
	for i := range 3 {
		a, err := l.Reserve(now, "k")
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		held = append(held, a)
	}
	if _, err := l.Reserve(now, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("fourth attempt in flight: %v, want ErrRateLimited", err)
	}

	held[0].Release()
	held[0].Release() // ending an attempt twice frees one slot only
	a, err := l.Reserve(now, "k")
	if err != nil {
		t.Fatalf("attempt after a release: %v", err)
	}
	held[0] = a
	if _, err := l.Reserve(now, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("attempt over the limit after a release: %v, want ErrRateLimited", err)
	}

	for i, want := range []bool{false, false, true} {
		if got := held[i].Fail(now); got != want {
			t.Fatalf("failure %d: Fail = %v, want %v", i+1, got, want)
		}
	}
	if !l.blocked("k", now) {
		t.Fatal("three failed attempts did not block the key")
	}
	if _, err := l.Reserve(now, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("attempt on a blocked key: %v, want ErrRateLimited", err)
	}
}

func TestReserveIsAllOrNothing(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 2, Window: time.Minute, BlockWindow: time.Minute})
	l.recordFailure("blocked", now)
	l.recordFailure("blocked", now)

	if _, err := l.Reserve(now, "free", "blocked"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("attempt with one blocked key: %v, want ErrRateLimited", err)
	}
	// The refused attempt took nothing from the free key.
	for i := range 2 {
		if _, err := l.Reserve(now, "free"); err != nil {
			t.Fatalf("slot %d of the free key: %v", i+1, err)
		}
	}
}

func TestAttemptInFlightSurvivesExpiry(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 2, Window: time.Minute, BlockWindow: time.Hour})

	slow, err := l.Reserve(start, "k")
	if err != nil {
		t.Fatal(err)
	}
	later := start.Add(10 * time.Minute)
	if l.blocked("k", later) {
		t.Fatal("an attempt in flight blocked the key")
	}
	if _, ok := l.entries["k"]; !ok {
		t.Fatal("expiry dropped a key with an attempt in flight")
	}
	other, err := l.Reserve(later, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(later, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("third attempt while two are in flight: %v, want ErrRateLimited", err)
	}

	// A failure older than the window does not add up with a later one.
	if slow.Fail(later) {
		t.Fatal("first failure blocked the key")
	}
	if other.Fail(later.Add(2 * time.Minute)) {
		t.Fatal("a failure older than the window counted toward the block")
	}
}

func TestResetKeepsAttemptsInFlight(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 2, Window: time.Minute, BlockWindow: time.Minute})

	slow, err := l.Reserve(now, "k")
	if err != nil {
		t.Fatal(err)
	}
	l.recordFailure("k", now)
	if _, err := l.Reserve(now, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("attempt with one failure and one in flight: %v, want ErrRateLimited", err)
	}

	l.Reset("k") // clears the failure, not the attempt in flight
	if _, err := l.Reserve(now, "k"); err != nil {
		t.Fatalf("attempt after the reset: %v", err)
	}
	if _, err := l.Reserve(now, "k"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("third attempt while two are in flight: %v, want ErrRateLimited", err)
	}
	if slow.Fail(now) {
		t.Fatal("one failure after the reset blocked the key")
	}
}
