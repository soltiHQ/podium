package event

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	clientBufSize    = 16
	defaultMaxEvents = 100
)

// Hub broadcasts UI update notifications to all SSE clients and keeps separate ring buffers for activity events and issues.
type Hub struct {
	mu     sync.RWMutex
	logger zerolog.Logger

	clients map[uint64]chan string
	nextID  uint64
	closed  bool

	events *Ring[Record]
	issues *Ring[Record]
}

// NewHub creates a new notification hub.
func NewHub(logger zerolog.Logger) *Hub {
	return &Hub{
		logger:  logger,
		clients: make(map[uint64]chan string),
		events:  NewRing[Record](defaultMaxEvents),
		issues:  NewRing[Record](defaultMaxEvents),
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

	for id, ch := range h.clients {
		select {
		case ch <- event:
			h.logger.Trace().Uint64("client_id", id).Str("event", event).Msg("sse: event sent")
		default:
			h.logger.Warn().
				Uint64("client_id", id).
				Str("event", event).
				Msg("sse: dropped event, client buffer full")
		}
	}
}

// Record appends an event to the activity ring buffer.
func (h *Hub) Record(kind string, payload Payload) {
	rec := Record{
		Time:    time.Now(),
		Payload: payload,
		Kind:    kind,
	}

	h.events.Append(rec)
	if IsIssueKind(kind) {
		h.issues.Append(rec)
	}
}

// RecentEvents returns the last n activity events in reverse chronological order.
func (h *Hub) RecentEvents(n int) []Record {
	return h.events.Recent(n)
}

// RecentIssues returns the last n issues in reverse chronological order.
func (h *Hub) RecentIssues(n int) []Record {
	return h.issues.Recent(n)
}

// DeleteIssues removes all issues matching kind and payload ID.
func (h *Hub) DeleteIssues(kind, id string) int {
	return h.issues.DeleteFunc(func(ev Record) bool {
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
	total := len(h.clients)
	h.mu.Unlock()

	h.logger.Debug().Uint64("client_id", id).Int("clients", total).Msg("sse: client subscribed")

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if !h.closed {
			delete(h.clients, id)
			close(ch)
		}
		remaining := len(h.clients)
		h.mu.Unlock()
		h.logger.Debug().Uint64("client_id", id).Int("clients", remaining).Msg("sse: client unsubscribed")
	}()
	return ch
}
