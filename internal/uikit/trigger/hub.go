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

// EventRecord is a single entry in the ring buffer.
type EventRecord struct {
	Time    time.Time
	Kind    string
	Payload EventPayload
}

// Hub broadcasts UI update notifications to all SSE clients and keeps
// separate ring buffers for activity events and issues.
type Hub struct {
	mu sync.RWMutex

	clients map[uint64]chan string
	nextID  uint64
	closed  bool

	events *Ring[EventRecord]
	issues *Ring[EventRecord]
}

// NewHub creates a new notification hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[uint64]chan string),
		events:  NewRing[EventRecord](defaultMaxEvents),
		issues:  NewRing[EventRecord](defaultMaxEvents),
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

// Record appends an event to the activity ring buffer.
// Issue-classified events are also appended to the issues ring buffer.
func (h *Hub) Record(kind string, payload EventPayload) {
	rec := EventRecord{Time: time.Now(), Kind: kind, Payload: payload}
	h.events.Append(rec)
	if IsIssueKind(kind) {
		h.issues.Append(rec)
	}
}

// RecentEvents returns the last n activity events in reverse chronological order.
func (h *Hub) RecentEvents(n int) []EventRecord {
	return h.events.Recent(n)
}

// RecentIssues returns the last n issues in reverse chronological order.
func (h *Hub) RecentIssues(n int) []EventRecord {
	return h.issues.Recent(n)
}

// DeleteIssues removes all issues matching kind and payload ID.
// Returns the number of removed issues.
func (h *Hub) DeleteIssues(kind, id string) int {
	return h.issues.DeleteFunc(func(ev EventRecord) bool {
		return ev.Kind == kind && ev.Payload.ID == id
	})
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
