package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth"
)

// maxEntries bounds the keys one Limiter tracks. Past it, the least recently
// updated key without attempts in flight is evicted.
const maxEntries = 100_000

// Config defines rate limiting parameters.
//
// MaxAttempts specifies how many failed attempts block a key.
// Each failure counts only within Window of the previous one:
// a key whose last failure is older than Window starts over.
//
// BlockWindow specifies the duration of the block once
// the threshold is reached.
//
// Zero or negative values are replaced with safe defaults in New().
type Config struct {
	MaxAttempts int
	Window      time.Duration
	BlockWindow time.Duration
}

type entry struct {
	key          string
	failures     int
	inFlight     int
	lastFailure  time.Time
	blockedUntil time.Time
}

// Limiter provides an in-memory, concurrency-safe rate limiter
// for authentication attempts identified by an arbitrary string key.
//
// Failures are recorded directly with RecordFailure, or through an Attempt
// taken with Reserve, which also counts the attempt while it is in flight.
//
// A block ends BlockWindow after it started; failures of a key that never got
// blocked are forgotten Window after the last one. At most 100 000 keys are
// tracked; past that, the least recently updated key is evicted. A key with
// an attempt in flight is neither forgotten nor evicted.
//
// The limiter is process-local and does not provide distributed guarantees.
// State is not persisted and is lost on restart.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // of *entry, least recently updated first
	cfg     Config
}

// Attempt is an attempt admitted by [Limiter.Reserve]. It holds one slot on
// each of its keys until it ends with Fail or Release.
type Attempt struct {
	l    *Limiter
	keys []string
	done bool
}

// New creates a new Limiter with normalized configuration.
//
// If MaxAttempts <= 0, a default value is used.
// If Window or BlockWindow <= 0, a default value is used.
func New(cfg Config) *Limiter {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.Window <= 0 {
		cfg.Window = 10 * time.Minute
	}
	if cfg.BlockWindow <= 0 {
		cfg.BlockWindow = 10 * time.Minute
	}
	return &Limiter{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		cfg:     cfg,
	}
}

// Blocked reports whether the key is currently blocked at time now.
//
// Expired state is dropped first.
//
// Safe for concurrent use.
func (l *Limiter) Blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.live(key, now)
	return e != nil && now.Before(e.blockedUntil)
}

// RecordFailure records a failed attempt for the key and reports whether
// this failure blocked it.
//
// The failure that reaches MaxAttempts blocks the key until now + BlockWindow.
// A failure recorded while the key is blocked changes nothing.
//
// Safe for concurrent use.
func (l *Limiter) RecordFailure(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.fail(l.ensure(key, now), now)
}

// Reserve admits one attempt on all keys at once, or on none. It returns
// auth.ErrRateLimited when, for any key, the failures plus the attempts in
// flight reach MaxAttempts; a blocked key always does. The admitted attempt
// holds a slot on each key until it ends.
//
// Safe for concurrent use.
func (l *Limiter) Reserve(now time.Time, keys ...string) (*Attempt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, key := range keys {
		if e := l.live(key, now); e != nil && e.failures+e.inFlight >= l.cfg.MaxAttempts {
			return nil, auth.ErrRateLimited
		}
	}
	for _, key := range keys {
		l.ensure(key, now).inFlight++
		l.order.MoveToBack(l.entries[key])
	}
	return &Attempt{l: l, keys: keys}, nil
}

// Reset forgets the failures and any block of the key.
// Attempts in flight keep their slots.
//
// Safe for concurrent use.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	el, ok := l.entries[key]
	if !ok {
		return
	}
	if e := el.Value.(*entry); e.inFlight > 0 {
		e.failures, e.lastFailure, e.blockedUntil = 0, time.Time{}, time.Time{}
		return
	}
	l.remove(el)
}

// Fail ends the attempt as failed: it records a failure on each key, as
// RecordFailure does, and reports whether that blocked any of them.
// Ending an attempt that has ended does nothing.
func (a *Attempt) Fail(now time.Time) bool {
	l := a.l
	l.mu.Lock()
	defer l.mu.Unlock()

	if a.done {
		return false
	}
	a.done = true
	blocked := false
	for _, key := range a.keys {
		e := l.entries[key].Value.(*entry) // kept while the attempt is in flight
		e.inFlight--
		l.settle(e, now)
		if l.fail(e, now) {
			blocked = true
		}
	}
	return blocked
}

// Release ends the attempt without recording a failure.
// Ending an attempt that has ended does nothing.
func (a *Attempt) Release() {
	l := a.l
	l.mu.Lock()
	defer l.mu.Unlock()

	if a.done {
		return
	}
	a.done = true
	for _, key := range a.keys {
		el := l.entries[key] // kept while the attempt is in flight
		e := el.Value.(*entry)
		e.inFlight--
		if e.inFlight == 0 && e.failures == 0 {
			l.remove(el)
		}
	}
}

// fail records a failure on e and reports whether it blocked e.
// A blocked entry records nothing.
func (l *Limiter) fail(e *entry, now time.Time) bool {
	if now.Before(e.blockedUntil) {
		return false
	}
	l.order.MoveToBack(l.entries[e.key])
	e.failures++
	e.lastFailure = now
	if e.failures < l.cfg.MaxAttempts {
		return false
	}
	e.blockedUntil = now.Add(l.cfg.BlockWindow)
	return true
}

// ensure returns the live entry of key, adding an empty one when there is none.
func (l *Limiter) ensure(key string, now time.Time) *entry {
	if e := l.live(key, now); e != nil {
		return e
	}
	l.prune(now)
	el := l.order.PushBack(&entry{key: key})
	l.entries[key] = el
	l.evict(el)
	return el.Value.(*entry)
}

// live returns the entry of key without its expired state, or nil when
// nothing is left of it.
func (l *Limiter) live(key string, now time.Time) *entry {
	el, ok := l.entries[key]
	if !ok {
		return nil
	}
	e := el.Value.(*entry)
	if l.settle(e, now) {
		l.remove(el)
		return nil
	}
	return e
}

// settle forgets an ended block, or failures older than Window, and reports
// whether nothing is left of e.
func (l *Limiter) settle(e *entry, now time.Time) bool {
	switch {
	case !e.blockedUntil.IsZero():
		if !now.Before(e.blockedUntil) {
			e.failures, e.blockedUntil = 0, time.Time{}
		}
	case !now.Before(e.lastFailure.Add(l.cfg.Window)):
		e.failures = 0
	}
	return e.failures == 0 && e.inFlight == 0
}

// prune drops entries with nothing left from the least recently updated end.
func (l *Limiter) prune(now time.Time) {
	for el := l.order.Front(); el != nil && l.settle(el.Value.(*entry), now); el = l.order.Front() {
		l.remove(el)
	}
}

// evict drops the least recently updated entries without attempts in flight,
// sparing keep, while the limiter holds more than maxEntries keys.
func (l *Limiter) evict(keep *list.Element) {
	for el := l.order.Front(); el != nil && l.order.Len() > maxEntries; {
		next := el.Next()
		if el != keep && el.Value.(*entry).inFlight == 0 {
			l.remove(el)
		}
		el = next
	}
}

func (l *Limiter) remove(el *list.Element) {
	delete(l.entries, l.order.Remove(el).(*entry).key)
}
