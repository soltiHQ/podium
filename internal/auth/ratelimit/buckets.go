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

	full := e.full
	if full.Before(now) {
		full = now
	}
	full = full.Add(b.cfg.Refill)
	if full.Sub(now) <= time.Duration(b.cfg.Burst)*b.cfg.Refill {
		e.full = full
		return true, false
	}

	if e.noticed.IsZero() || now.Sub(e.noticed) >= b.cfg.Notice {
		e.noticed = now
		return false, true
	}
	return false, false
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
