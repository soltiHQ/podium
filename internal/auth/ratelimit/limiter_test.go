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

	if l.Blocked("k", time.Now()) {
		t.Fatal("expected not blocked initially")
	}
}

func TestLimiter_BlockAfterThreshold(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 3, BlockWindow: time.Minute})

	l.RecordFailure("k", now)
	l.RecordFailure("k", now)
	if l.Blocked("k", now) {
		t.Fatal("should not be blocked before threshold")
	}

	l.RecordFailure("k", now)
	if !l.Blocked("k", now) {
		t.Fatal("expected blocked after reaching threshold")
	}
}

func TestLimiter_CheckReturnsErrorWhenBlocked(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.RecordFailure("k", now)

	err := l.Check("k", now)
	if !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestLimiter_BlockExpires(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.RecordFailure("k", start)

	if !l.Blocked("k", start) {
		t.Fatal("expected blocked immediately")
	}

	after := start.Add(2 * time.Minute)

	if l.Blocked("k", after) {
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

	l.RecordFailure("k", now)

	if !l.Blocked("k", now) {
		t.Fatal("expected blocked")
	}

	l.Reset("k")

	if l.Blocked("k", now) {
		t.Fatal("expected not blocked after reset")
	}
}

func TestLimiter_IndependentKeys(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.RecordFailure("k1", now)

	if !l.Blocked("k1", now) {
		t.Fatal("k1 should be blocked")
	}
	if l.Blocked("k2", now) {
		t.Fatal("k2 should not be blocked")
	}
}

func TestLimiter_FailuresAccumulateUntilReset(t *testing.T) {
	now := time.Now()
	l := New(Config{MaxAttempts: 3, BlockWindow: time.Minute})

	l.RecordFailure("k", now)
	l.RecordFailure("k", now)

	if l.Blocked("k", now) {
		t.Fatal("should not be blocked yet")
	}

	l.RecordFailure("k", now)

	if !l.Blocked("k", now) {
		t.Fatal("expected blocked at exactly threshold")
	}
}

func TestLimiter_ExpiryResetsFailureState(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 1, BlockWindow: time.Minute})

	l.RecordFailure("k", start)

	after := start.Add(2 * time.Minute)
	if l.Blocked("k", after) {
		t.Fatal("block should have expired")
	}

	l.RecordFailure("k", after)
	if !l.Blocked("k", after) {
		t.Fatal("should block again after new failure")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	const limit = 100_000
	now := time.Now()
	l := New(Config{MaxAttempts: 5, BlockWindow: time.Hour})

	l.RecordFailure("first", now)
	l.RecordFailure("second", now)
	for i := range limit - 2 {
		l.RecordFailure("filler-"+strconv.Itoa(i), now)
	}
	l.RecordFailure("first", now) // "second" is now the least recently updated key
	l.RecordFailure("overflow", now)

	if n := len(l.entries); n > limit {
		t.Fatalf("limiter holds %d keys, want at most %d", n, limit)
	}
	if _, ok := l.entries["second"]; ok {
		t.Fatal("the least recently updated key was not evicted")
	}
	for _, key := range []string{"first", "overflow"} {
		if _, ok := l.entries[key]; !ok {
			t.Fatalf("recently updated key %q was evicted", key)
		}
	}
}

func TestRecordFailureReportsTheBlockOnce(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 3, Window: time.Minute, BlockWindow: time.Minute})

	for _, at := range []time.Time{start, start.Add(2 * time.Minute)} {
		for i, want := range []bool{false, false, true, false} {
			if got := l.RecordFailure("k", at); got != want {
				t.Fatalf("failure %d at %v: RecordFailure = %v, want %v", i+1, at.Sub(start), got, want)
			}
		}
		if !l.Blocked("k", at) {
			t.Fatalf("not blocked at %v", at.Sub(start))
		}
	}
}

func TestFailuresCountWithinWindowOfTheLastOne(t *testing.T) {
	start := time.Now()
	l := New(Config{MaxAttempts: 3, Window: time.Minute, BlockWindow: time.Hour})

	// Each failure lands within a minute of the previous one.
	l.RecordFailure("k", start)
	l.RecordFailure("k", start.Add(50*time.Second))
	l.RecordFailure("k", start.Add(100*time.Second))
	if !l.Blocked("k", start.Add(100*time.Second)) {
		t.Fatal("failures within the window of each other did not block")
	}
	if !l.Blocked("k", start.Add(100*time.Second+59*time.Minute)) {
		t.Fatal("block ended before BlockWindow")
	}
	if l.Blocked("k", start.Add(100*time.Second+time.Hour)) {
		t.Fatal("block outlived BlockWindow")
	}

	// Failures further apart than the window never add up.
	for i := range 5 {
		l.RecordFailure("spaced", start.Add(time.Duration(i)*61*time.Second))
	}
	if l.Blocked("spaced", start.Add(5*61*time.Second)) {
		t.Fatal("failures more than a window apart added up")
	}
	if l.Blocked("spaced", start.Add(10*time.Minute)) {
		t.Fatal("unblocked key reported blocked after its window")
	}
	if _, ok := l.entries["spaced"]; ok {
		t.Fatal("expected the entry to be removed a window after its last failure")
	}
}
