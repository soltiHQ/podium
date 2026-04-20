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

// Writer routes mutating hub operations (Notify, Record, DeleteIssues)
// through an external transport (Raft, Kafka, etc.) so every replica sees
// the same events. Nil writer = in-process only (standalone profile).
type Writer interface {
	Notify(event string)
	Record(kind string, payload Payload)
	DeleteIssues(kind, id string) int
}

// Hub broadcasts UI update notifications to all SSE clients and keeps separate ring buffers for activity events and issues.
//
// Writes (Notify/Record/DeleteIssues) delegate to an external Writer when
// one is attached (see SetWriter) — that transport is expected to call back
// into ApplyLocal* on every replica to keep state consistent. Reads (Recent*,
// Subscribe, SSEHandler) are always local.
type Hub struct {
	mu     sync.RWMutex
	logger zerolog.Logger

	clients map[uint64]chan string
	nextID  uint64
	closed  bool

	events *Ring[Record]
	issues *Ring[Record]

	writer Writer
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

// SetWriter attaches a replication transport. Must be called once before
// any Notify/Record/DeleteIssues — typically by the cluster profile at
// startup. Pass nil to detach.
func (h *Hub) SetWriter(w Writer) { h.writer = w }

// Notify broadcasts an event name. When a Writer is attached, routes
// through it (replicated); otherwise fires locally.
func (h *Hub) Notify(event string) {
	if h.writer != nil {
		h.writer.Notify(event)
		return
	}
	h.ApplyLocalNotify(event)
}

// ApplyLocalNotify fires the notify locally without going through the
// Writer. Called by the replication transport on every replica.
func (h *Hub) ApplyLocalNotify(event string) {
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

// Record appends an event to the activity ring buffer. When a Writer is
// attached, routes through it.
func (h *Hub) Record(kind string, payload Payload) {
	if h.writer != nil {
		h.writer.Record(kind, payload)
		return
	}
	h.ApplyLocalRecord(kind, payload)
}

// ApplyLocalRecord appends to the ring buffer without going through the
// Writer. Called by the replication transport on every replica.
func (h *Hub) ApplyLocalRecord(kind string, payload Payload) {
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

// DeleteIssues removes all issues matching (kind, id). Routes through the
// Writer when attached; returns the local removal count. (Cluster-wide
// count is not surfaced — matches the original single-node signature.)
func (h *Hub) DeleteIssues(kind, id string) int {
	if h.writer != nil {
		return h.writer.DeleteIssues(kind, id)
	}
	return h.ApplyLocalDeleteIssues(kind, id)
}

// ApplyLocalDeleteIssues drops matching entries from the local ring without
// going through the Writer. Called by the replication transport on every
// replica.
func (h *Hub) ApplyLocalDeleteIssues(kind, id string) int {
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
