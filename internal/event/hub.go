package event

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const clientBufSize = 16

// RingSize is how many recent activity events, and apart from them how many
// issues, a Hub keeps.
const RingSize = 100

// Writer routes mutating hub operations (Notify, Record, DeleteIssues)
// through an external transport (Raft, Kafka, etc.) so every replica sees
// the same events. Nil writer = in-process only (memory backend).
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
//
// With a window (see WithCoalesce) the refresh pulses that reach the clients
// are coalesced, see ApplyLocalNotify.
type Hub struct {
	mu     sync.RWMutex
	logger zerolog.Logger

	clients map[uint64]chan string
	nextID  uint64
	closed  bool

	events *Ring[Record]
	issues *Ring[Record]

	writer   Writer
	coalesce *coalescer // nil: every notification is delivered
}

// Option configures a Hub.
type Option func(*Hub)

// WithCoalesce makes the hub coalesce refresh pulses (see IsRefreshKind): of
// each kind its clients get at most one per window. The first pulse after a
// quiet period is delivered at once; those that follow within the window are
// held back as one trailing delivery at its end, so a burst costs the clients
// two deliveries and its last change is never lost. A window of zero or less,
// which a hub built without this option also has, delivers every pulse.
func WithCoalesce(window time.Duration) Option {
	return func(h *Hub) {
		if window > 0 {
			h.coalesce = &coalescer{window: window, deliver: h.broadcast, pulses: make(map[string]*pulse)}
		}
	}
}

// NewHub creates a new notification hub.
func NewHub(logger zerolog.Logger, opts ...Option) *Hub {
	h := &Hub{
		logger:  logger,
		clients: make(map[uint64]chan string),
		events:  NewRing[Record](RingSize),
		issues:  NewRing[Record](RingSize),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Close disconnects all SSE clients by closing their channels, and stops the
// trailing deliveries that wait on a timer.
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
	if h.coalesce != nil {
		h.coalesce.stop()
	}
}

// SetWriter attaches a replication transport. Must be called once before
// any Notify/Record/DeleteIssues — typically by the cluster profile at
// startup. Pass nil to detach.
func (h *Hub) SetWriter(w Writer) { h.writer = w }

// Notify broadcasts an event name. When a Writer is attached, routes
// through it (replicated); otherwise fires locally. Notify itself holds
// nothing back: see ApplyLocalNotify.
func (h *Hub) Notify(event string) {
	if h.writer != nil {
		h.writer.Notify(event)
		return
	}
	h.ApplyLocalNotify(event)
}

// ApplyLocalNotify fires the notify locally without going through the
// Writer. Called by the replication transport on every replica.
//
// Here a window (see WithCoalesce) holds refresh pulses back, and not in
// Notify, because this is where every pulse reaches the clients of a replica:
// from a Notify of the memory backend, from the state machine of each Raft
// replica that applies a replicated pulse, and from the local fallback of a
// follower. Each replica thus bounds the traffic to its own clients, whichever
// node sent the burst and whatever leadership did meanwhile, and the trailing
// delivery comes from the pulses that replica applied. It only touches local
// channels, so no timer ever calls into the replication transport. The pulses
// Notify sends to the Writer are not held back: replication costs one command
// per pulse.
func (h *Hub) ApplyLocalNotify(event string) {
	if h.coalesce != nil && IsRefreshKind(event) {
		h.coalesce.notify(event)
		return
	}
	h.broadcast(event)
}

// broadcast sends the event to every client, and drops it for a client whose
// buffer is full.
func (h *Hub) broadcast(event string) {
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

// coalescer delivers the refresh pulses of a hub at most once per window and
// kind: the first after a quiet period at once, and what arrives within the
// window as one trailing delivery at its end. A pulse only tells a client to
// fetch again, so the trailing delivery stands for every pulse it replaces. Its
// timer is all the coalescer starts, and stop ends it.
//
// Only refresh kinds are coalesced (see IsRefreshKind), so the state is bounded
// by their number.
type coalescer struct {
	window  time.Duration
	deliver func(kind string)

	mu      sync.Mutex
	pulses  map[string]*pulse // by kind
	stopped bool
}

// pulse is what the coalescer knows of one refresh kind.
type pulse struct {
	last  time.Time   // when the clients last got it
	timer *time.Timer // the trailing delivery that waits, if any
}

// notify delivers the pulse of the kind, or holds it back. It never waits: a
// pulse inside the window sets a timer, or finds that one is set already.
func (c *coalescer) notify(kind string) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	p := c.pulses[kind]
	if p == nil {
		p = &pulse{}
		c.pulses[kind] = p
	}
	now := time.Now()
	switch {
	case p.timer != nil:
		// The trailing delivery that waits comes after this pulse, so it covers it.
	case now.Sub(p.last) >= c.window:
		p.last = now
		c.mu.Unlock()
		c.deliver(kind)
		return
	default:
		p.timer = time.AfterFunc(c.window-now.Sub(p.last), func() { c.trail(kind) })
	}
	c.mu.Unlock()
}

// trail is the trailing delivery of the kind, at the end of its window. It
// starts the next window.
func (c *coalescer) trail(kind string) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	p := c.pulses[kind]
	p.timer, p.last = nil, time.Now()
	c.mu.Unlock()
	c.deliver(kind)
}

// stop cancels the trailing deliveries that wait, and refuses to set more. One
// whose timer fired already returns without delivering.
func (c *coalescer) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for _, p := range c.pulses {
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
	}
}
