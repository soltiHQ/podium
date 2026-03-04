package trigger

import (
	"io"
	"net/http"
	"time"
)

// SSEHandler returns an http.HandlerFunc that streams UI update notifications as Server-Sent Events.
func SSEHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			http.Error(w, "cannot disable write deadline", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch := Subscribe(r.Context())
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				io.WriteString(w, "data: "+ev+"\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}
