package event

import (
	"io"
	"net/http"
	"time"
)

// SSEHandler returns an http.HandlerFunc that streams UI update notifications as Server-Sent Events.
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
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if _, err := io.WriteString(w, "data: "+ev+"\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}
