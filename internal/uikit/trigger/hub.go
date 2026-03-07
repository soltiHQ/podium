package trigger

import (
	"context"
	"sync"
	"time"
)

const (
	clientBufSize    = 16
	defaultMaxEvents = 100
)

// EventPayload describes who did what to whom.
type EventPayload struct {
	ID     string
	Name   string
	By     string
	Detail string
}

// EventRecord is a single entry in the recent-activity ring buffer.
type EventRecord struct {
	Time    time.Time
	Kind    string
	Payload EventPayload
}

// Hub broadcasts UI update notifications to all clients and keeps
// a ring buffer of the most recent events for the dashboard feed.
type Hub struct {
	mu sync.RWMutex

	clients map[uint64]chan string
	nextID  uint64
	closed  bool

	events    []EventRecord
	maxEvents int
}

// NewHub creates a new notification hub.
func NewHub() *Hub {
	return &Hub{
		clients:   make(map[uint64]chan string),
		maxEvents: defaultMaxEvents,
	}
}

// Close disconnects all SSE clients by closing their channels.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return
	}
	h.closed = true

	for id, ch := range h.clients {
		delete(h.clients, id)
		close(ch)
	}
}

// Notify sends an event name to every connected client.
func (h *Hub) Notify(event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.closed {
		return
	}

	for _, ch := range h.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

// Record appends an event to the ring buffer.
func (h *Hub) Record(kind string, payload EventPayload) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.events = append(h.events, EventRecord{
		Time:    time.Now(),
		Kind:    kind,
		Payload: payload,
	})

	if len(h.events) > h.maxEvents {
		h.events = h.events[len(h.events)-h.maxEvents:]
	}
}

// RecentEvents returns the last n events in reverse chronological order.
func (h *Hub) RecentEvents(n int) []EventRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()

	total := len(h.events)
	if n <= 0 || total == 0 {
		return nil
	}
	if n > total {
		n = total
	}

	out := make([]EventRecord, n)
	for i := range n {
		out[i] = h.events[total-1-i]
	}
	return out
}

// RecentEventsOfKind returns the last n events matching any of the given kinds, in reverse chronological order.
func (h *Hub) RecentEventsOfKind(n int, kinds ...string) []EventRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()

	kindSet := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		kindSet[k] = struct{}{}
	}

	var out []EventRecord
	for i := len(h.events) - 1; i >= 0 && len(out) < n; i-- {
		if _, ok := kindSet[h.events[i].Kind]; ok {
			out = append(out, h.events[i])
		}
	}
	return out
}

// Subscribe registers a new listener.
func (h *Hub) Subscribe(ctx context.Context) <-chan string {
	ch := make(chan string, clientBufSize)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch
	}

	id := h.nextID
	h.nextID++
	h.clients[id] = ch
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if !h.closed {
			delete(h.clients, id)
			close(ch)
		}
		h.mu.Unlock()
	}()
	return ch
}
