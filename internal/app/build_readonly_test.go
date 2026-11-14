package app

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// readOnlySpy records whether the agent listing, and the transactions, of the
// store it wraps got a context marked read-only.
type readOnlySpy struct {
	storage.Storage

	mu         sync.Mutex
	listings   []bool
	transacted []bool
}

func (s *readOnlySpy) ListAgents(ctx context.Context, filter storage.AgentFilter, opts storage.ListOptions) (*storage.AgentListResult, error) {
	s.mu.Lock()
	s.listings = append(s.listings, storage.IsReadOnly(ctx))
	s.mu.Unlock()
	return s.Storage.ListAgents(ctx, filter, opts)
}

func (s *readOnlySpy) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	s.mu.Lock()
	s.transacted = append(s.transacted, storage.IsReadOnly(ctx))
	s.mu.Unlock()
	return s.Storage.WithTx(ctx, fn)
}

// marks returns what the spy recorded since the last call, and forgets it.
func (s *readOnlySpy) marks() (listings, transacted []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listings, transacted = s.listings, s.transacted
	s.listings, s.transacted = nil, nil
	return listings, transacted
}

// TestMainHandlerMarksGetRequestsReadOnly checks that the UI and API listener
// marks a GET request's context read-only, so that a backend with a read
// replica may serve its listings, and leaves a DELETE's unmarked.
func TestMainHandlerMarksGetRequestsReadOnly(t *testing.T) {
	var spy *readOnlySpy
	f := newBrowserFixture(t, config.Default(), func(s storage.Storage) storage.Storage {
		spy = &readOnlySpy{Storage: s}
		return spy
	})
	alice := f.login(t, "alice")
	spy.marks()

	if w := f.serve(browserRequest(http.MethodGet, "/api/v1/agents", "", "", alice)); w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/agents: HTTP %d, want 200", w.Code)
	}
	if listings, _ := spy.marks(); len(listings) != 1 || !listings[0] {
		t.Errorf("GET /api/v1/agents listed the agents with marks %v, want one listing, marked read-only", listings)
	}

	if w := f.serve(browserRequest(http.MethodDelete, "/api/v1/agents/agent-gone", "", "", alice)); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE /api/v1/agents/agent-gone: HTTP %d, want 404", w.Code)
	}
	if _, transacted := spy.marks(); len(transacted) == 0 || slices.Contains(transacted, true) {
		t.Errorf("DELETE /api/v1/agents/agent-gone ran transactions with marks %v, want its delete's, unmarked", transacted)
	}
}
