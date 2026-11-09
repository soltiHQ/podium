package ops

import (
	"encoding/json"
	"net/http"

	"github.com/soltiHQ/control-plane/internal/buildinfo"
)

// notLeader is what /leaderz says of a node that serves but does not lead.
const notLeader = "not leader"

// Probes are what the ops routes report on.
type Probes struct {
	// Ready returns "" while the node is ready, and otherwise the one-line
	// reason it is not.
	Ready func() string
	// Leader reports whether the node is the leader; a standalone node always is.
	Leader func() bool
	// Version is what /version reports.
	Version buildinfo.Info
}

// Handler serves the ops routes. Every route answers GET and HEAD, never
// authenticates and logs nothing per request: supervisors probe it often.
type Handler struct {
	mux *http.ServeMux
}

// NewHandler returns the handler of the routes /livez, /readyz, /leaderz and
// /version over p. It panics on a Probes without Ready or Leader.
func NewHandler(p Probes) *Handler {
	if p.Ready == nil || p.Leader == nil {
		panic("ops: probes need Ready and Leader")
	}
	h := &Handler{mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeLine(w, http.StatusOK, "ok")
	})
	h.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if reason := p.Ready(); reason != "" {
			writeLine(w, http.StatusServiceUnavailable, reason)
			return
		}
		writeLine(w, http.StatusOK, "ok")
	})
	h.mux.HandleFunc("GET /leaderz", func(w http.ResponseWriter, _ *http.Request) {
		switch reason := p.Ready(); {
		case reason != "":
			writeLine(w, http.StatusServiceUnavailable, reason)
		case !p.Leader():
			writeLine(w, http.StatusServiceUnavailable, notLeader)
		default:
			writeLine(w, http.StatusOK, "ok")
		}
	})
	body, err := json.Marshal(p.Version)
	if err != nil {
		panic("ops: marshal the version: " + err.Error())
	}
	h.mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append(body, '\n'))
	})
	return h
}

// Handle registers handler for pattern, a [http.ServeMux] pattern such as
// "GET /metrics", on the ops listener. It must run before the listener serves.
func (h *Handler) Handle(pattern string, handler http.Handler) {
	h.mux.Handle(pattern, handler)
}

// ServeHTTP serves the ops routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// writeLine answers with one line of text.
func writeLine(w http.ResponseWriter, status int, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(line + "\n"))
}
