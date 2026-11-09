package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// BucketConfig defines the parameters of [Buckets], see [NewBuckets].
type BucketConfig struct {
	// Burst is what a full bucket holds, and so how many takes in a row succeed
	// on a key that has not been used.
	Burst int

	// Refill is how long a token takes to come back: one per Refill.
	Refill time.Duration

	// Idle is how long a key may go without a take before its bucket is dropped.
	Idle time.Duration

	// Notice is the shortest time between two refusals of a key that ask to be
	// logged, see [Buckets.Take]. Zero asks on every refusal.
	Notice time.Duration
}

type bucket struct {
	key     string
	full    time.Time // when the bucket is full again; before now, it is full
	used    time.Time // the last take of the key, refused or not
	noticed time.Time // the last refusal that asked to be logged
}

// Buckets limits how often each of many keys may act: a token bucket per key,
// which holds up to Burst tokens and gets one back every Refill. A take spends
// a token. With none left it is refused and spends nothing, so a key that keeps
// asking is given a token as soon as the next one is due.
//
// A bucket is kept as the time at which it is full again, one timestamp per
// key: each take moves it Refill later, and a bucket that is more than
// Burst*Refill ahead of now has no token. Time that is not a whole Refill
// counts toward the next token.
//
// A key not used for Idle is dropped, so memory follows the keys in use. A
// dropped key starts again with a full bucket, which is what a bucket idle for
// that long is.
//
// The set is process-local and does not provide distributed guarantees. State
// is not persisted and is lost on restart.
type Buckets struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // of *bucket, least recently used first
	cfg     BucketConfig
}

// NewBuckets creates a set of buckets with cfg. It panics when Burst or Refill
// is not positive, which no caller can mean. Idle is raised to Burst*Refill and
// to Notice when it is shorter: a key is dropped only once its bucket is full
// again and it has no refusal left to remember.
func NewBuckets(cfg BucketConfig) *Buckets {
	if cfg.Burst < 1 || cfg.Refill <= 0 {
		panic("ratelimit.NewBuckets: Burst and Refill must be positive")
	}
	cfg.Idle = max(cfg.Idle, time.Duration(cfg.Burst)*cfg.Refill, cfg.Notice)
	return &Buckets{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		cfg:     cfg,
	}
}

// Take spends one token of the bucket of key at time now and reports whether it
// did. The first take of a key finds a full bucket.
//
// notice is true for a refusal that should be logged: the first of the key, and
// then the first after Notice since the last one that asked. It is false for a
// take that got a token and for every other refusal, so a key that keeps asking
// for a token it does not get costs the log one line per Notice.
//
// Safe for concurrent use.
func (b *Buckets) Take(now time.Time, key string) (ok, notice bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prune(now)
	var e *bucket
	if el, found := b.entries[key]; found {
		e = el.Value.(*bucket)
		b.order.MoveToBack(el)
	} else {
		e = &bucket{key: key}
		b.entries[key] = b.order.PushBack(e)
	}
	if now.After(e.used) {
		e.used = now
	}

	if full, has := b.spend(e, now); has {
		e.full = full
		return true, false
	}
	return false, b.asks(e, now)
}

// Peek reports whether a take of key at time now would get a token, without
// taking it. A key it has not seen has a full bucket, and a peek does not track
// it; nor is a peek a use of a key, which is dropped after Idle all the same. So
// callers that have not authenticated cannot make the set grow, nor keep a key
// in it.
//
// notice is as for Take, and shares its interval: a peek that finds the bucket
// empty asks to be logged when a take would have, and then neither asks again
// until Notice has passed.
//
// Safe for concurrent use.
func (b *Buckets) Peek(now time.Time, key string) (ok, notice bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	el, found := b.entries[key]
	if !found {
		return true, false
	}
	e := el.Value.(*bucket)
	if _, has := b.spend(e, now); has {
		return true, false
	}
	return false, b.asks(e, now)
}

// spend returns the time at which e is full again once a token is taken at now,
// and whether it has one to take.
func (b *Buckets) spend(e *bucket, now time.Time) (full time.Time, ok bool) {
	full = e.full
	if full.Before(now) {
		full = now
	}
	full = full.Add(b.cfg.Refill)
	return full, full.Sub(now) <= time.Duration(b.cfg.Burst)*b.cfg.Refill
}

// asks reports whether a refusal of e at now should be logged, and if so
// counts it as the one that asked.
func (b *Buckets) asks(e *bucket, now time.Time) bool {
	if e.noticed.IsZero() || now.Sub(e.noticed) >= b.cfg.Notice {
		e.noticed = now
		return true
	}
	return false
}

// prune drops the keys not used for Idle, from the least recently used end.
func (b *Buckets) prune(now time.Time) {
	for el := b.order.Front(); el != nil; el = b.order.Front() {
		e := el.Value.(*bucket)
		if now.Sub(e.used) < b.cfg.Idle {
			return
		}
		delete(b.entries, e.key)
		b.order.Remove(el)
	}
}
