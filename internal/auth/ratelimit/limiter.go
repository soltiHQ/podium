package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth"
)

// maxEntries bounds the keys one Limiter tracks.
// Past it, the least recently updated key is evicted.
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
	lastFailure  time.Time
	blockedUntil time.Time
}

// Limiter provides an in-memory, concurrency-safe rate limiter
// for authentication attempts identified by an arbitrary string key.
//
// An entry expires BlockWindow after its block started, or Window after
// its last failure when it never got blocked. At most 100 000 keys are
// tracked; past that, the least recently updated key is evicted.
//
// The limiter is process-local and does not provide distributed guarantees.
// State is not persisted and is lost on restart.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // of *entry, least recently updated first
	cfg     Config
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

// Check returns auth.ErrRateLimited if the key is currently blocked.
func (l *Limiter) Check(key string, now time.Time) error {
	if l.Blocked(key, now) {
		return auth.ErrRateLimited
	}
	return nil
}

// Blocked reports whether the key is currently blocked at time now.
//
// An expired entry is removed and reported as not blocked.
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

	e := l.live(key, now)
	switch {
	case e == nil:
		l.prune(now)
		e = &entry{key: key}
		l.entries[key] = l.order.PushBack(e)
		if l.order.Len() > maxEntries {
			l.remove(l.order.Front())
		}
	case now.Before(e.blockedUntil):
		return false
	default:
		l.order.MoveToBack(l.entries[key])
	}

	e.failures++
	e.lastFailure = now
	if e.failures < l.cfg.MaxAttempts {
		return false
	}
	e.blockedUntil = now.Add(l.cfg.BlockWindow)
	return true
}

// Reset removes all rate-limit state associated with the key.
//
// Safe for concurrent use.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.entries[key]; ok {
		l.remove(el)
	}
}

// live returns the entry of key, or nil when there is none or it has expired.
func (l *Limiter) live(key string, now time.Time) *entry {
	el, ok := l.entries[key]
	if !ok {
		return nil
	}
	e := el.Value.(*entry)
	if l.expired(e, now) {
		l.remove(el)
		return nil
	}
	return e
}

func (l *Limiter) expired(e *entry, now time.Time) bool {
	if !e.blockedUntil.IsZero() {
		return !now.Before(e.blockedUntil)
	}
	return !now.Before(e.lastFailure.Add(l.cfg.Window))
}

// prune drops expired entries from the least recently updated end.
func (l *Limiter) prune(now time.Time) {
	for el := l.order.Front(); el != nil && l.expired(el.Value.(*entry), now); el = l.order.Front() {
		l.remove(el)
	}
}

func (l *Limiter) remove(el *list.Element) {
	delete(l.entries, l.order.Remove(el).(*entry).key)
}
