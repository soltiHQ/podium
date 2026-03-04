package trigger

import (
	"context"
	"sync"
)

const clientBufSize = 16

// Hub broadcasts UI update notifications to all clients.
type Hub struct {
	mu sync.RWMutex

	clients map[uint64]chan string
	nextID  uint64
	closed  bool
}

// NewHub creates a new notification hub.
func NewHub() *Hub {
	return &Hub{clients: make(map[uint64]chan string)}
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
