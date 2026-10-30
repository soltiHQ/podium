package event

import (
	"io"
	"net/http"
	"time"
)

// streamKeepalive is how long the event stream may stay silent before it writes
// an SSE comment. A reverse proxy closes a connection it sees no bytes on
// (nginx proxy_read_timeout defaults to 60s); EventSource ignores comments.
// It is also how often the stream re-checks access, and half the time a write
// may take. It must be positive; tests shorten it.
// The task log stream keeps its own copy: internal/handler streamKeepalive.
var streamKeepalive = 15 * time.Second

// sseKeepalive is the comment a silent event stream writes.
const sseKeepalive = ": keepalive\n\n"

// SSEHandler returns an http.HandlerFunc that streams UI update notifications as Server-Sent Events.
//
// Before every write the stream sets the write deadline of its connection to 2×streamKeepalive
// ahead: the server's write timeout does not end it, but a client that stops reading is dropped.
// It writes a keepalive comment after streamKeepalive without output and, every streamKeepalive
// whether it wrote or not, calls access, which re-checks the caller's access (authentication, like
// the route): an error ends the stream, and EventSource reconnects through authentication.
// SSEHandler panics on a nil access.
func (h *Hub) SSEHandler(access func(*http.Request) error) http.HandlerFunc {
	if access == nil {
		panic(ErrNilAccess)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, ErrSSENotSupported.Error(), http.StatusInternalServerError)
			return
		}
		var (
			keepalive = streamKeepalive
			rc        = http.NewResponseController(w)
		)
		// deadline gives the next write 2×keepalive: a write at least every
		// keepalive keeps a reading client connected.
		deadline := func() error { return rc.SetWriteDeadline(time.Now().Add(2 * keepalive)) }
		if err := deadline(); err != nil {
			http.Error(w, ErrSSEDeadline.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher.Flush()

		ch := h.Subscribe(r.Context())
		idle := time.NewTicker(keepalive)
		defer idle.Stop()
		recheck := time.NewTicker(keepalive)
		defer recheck.Stop()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if deadline() != nil {
					return
				}
				if _, err := io.WriteString(w, "data: "+ev+"\n\n"); err != nil {
					return
				}
				idle.Reset(keepalive)
			case <-idle.C:
				if deadline() != nil {
					return
				}
				if _, err := io.WriteString(w, sseKeepalive); err != nil {
					return
				}
			case <-recheck.C:
				if err := access(r); err != nil {
					h.logger.Debug().Err(err).Msg("sse: access lost, stream ended")
					return
				}
				continue
			case <-r.Context().Done():
				return
			}
			flusher.Flush()
		}
	}
}
