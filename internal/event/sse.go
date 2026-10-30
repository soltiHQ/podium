package event

import (
	"io"
	"net/http"
	"time"
)

// streamKeepalive is how long the event stream may stay silent before it writes
// an SSE comment. A reverse proxy closes a connection it sees no bytes on
// (nginx proxy_read_timeout defaults to 60s); EventSource ignores comments.
// It must be positive; tests shorten it.
var streamKeepalive = 15 * time.Second

// sseKeepalive is the comment a silent event stream writes.
const sseKeepalive = ": keepalive\n\n"

// SSEHandler returns an http.HandlerFunc that streams UI update notifications as Server-Sent Events.
// The stream clears the write deadline of its connection, so the server's write timeout does not
// end it, and writes a keepalive comment after streamKeepalive without output.
func (h *Hub) SSEHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, ErrSSENotSupported.Error(), http.StatusInternalServerError)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			http.Error(w, ErrSSEDeadline.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch := h.Subscribe(r.Context())
		keepalive := streamKeepalive
		idle := time.NewTicker(keepalive)
		defer idle.Stop()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if _, err := io.WriteString(w, "data: "+ev+"\n\n"); err != nil {
					return
				}
				idle.Reset(keepalive)
			case <-idle.C:
				if _, err := io.WriteString(w, sseKeepalive); err != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
			flusher.Flush()
		}
	}
}
