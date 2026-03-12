package event

import "sync"

// Ring is a thread-safe capped ring buffer.
type Ring[T any] struct {
	mu  sync.RWMutex
	buf []T
	max int
}

// NewRing creates a ring buffer with the given capacity.
func NewRing[T any](max int) *Ring[T] {
	return &Ring[T]{max: max}
}

// Append adds an item, evicting the oldest when full.
func (r *Ring[T]) Append(item T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buf = append(r.buf, item)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

// Recent returns the last n items in reverse chronological order.
func (r *Ring[T]) Recent(n int) []T {
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := len(r.buf)
	if n <= 0 || total == 0 {
		return nil
	}
	if n > total {
		n = total
	}

	out := make([]T, n)
	for i := range n {
		out[i] = r.buf[total-1-i]
	}
	return out
}

// DeleteFunc removes items matching the predicate and returns the count.
func (r *Ring[T]) DeleteFunc(match func(T) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	filtered := r.buf[:0]
	for _, item := range r.buf {
		if match(item) {
			n++
			continue
		}
		filtered = append(filtered, item)
	}
	r.buf = filtered
	return n
}
