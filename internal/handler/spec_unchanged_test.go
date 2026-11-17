package handler

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
)

// recordingWriter keeps what a hub is asked to send, in place of a replication
// transport.
type recordingWriter struct {
	mu       sync.Mutex
	notified []string
	recorded []string
}

func (w *recordingWriter) Notify(kind string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notified = append(w.notified, kind)
}

func (w *recordingWriter) Record(kind string, _ event.Payload) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recorded = append(w.recorded, kind)
}

func (w *recordingWriter) DeleteIssues(string, string) int { return 0 }

// sent returns what was notified and recorded since the last call, and forgets it.
func (w *recordingWriter) sent() (notified, recorded []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	notified, recorded = w.notified, w.recorded
	w.notified, w.recorded = nil, nil
	return notified, recorded
}

// A save that changes something records spec_updated and refreshes the spec
// pages; one that changes nothing, which writes nothing, does neither, and
// still answers as a save does.
func TestSpecSaveThatChangesNothingRecordsNothing(t *testing.T) {
	admin := &identity.Identity{UserID: "admin", Permissions: enum.All}
	f := newAPIFixture(t)
	seedHandlerSchema(t, f.store)
	f.spec(t, "spec-1")
	sent := &recordingWriter{}
	f.hub.SetWriter(sent)
	save := func(version int) string {
		return fmt.Sprintf(`{"version":%d,"name":"Spec spec-1",%s}`, version, validKind)
	}

	if w := f.do(http.MethodPut, "/api/v1/specs/spec-1", save(1), admin); w.Code != http.StatusNoContent {
		t.Fatalf("the first save: HTTP %d %s", w.Code, w.Body.String())
	}
	notified, recorded := sent.sent()
	if !slices.Equal(recorded, []string{event.SpecUpdated}) || !slices.Contains(notified, event.RefreshSpecs) {
		t.Fatalf("a save that changed the spec recorded %v and notified %v", recorded, notified)
	}

	w := f.do(http.MethodPut, "/api/v1/specs/spec-1", save(2), admin)
	if w.Code != http.StatusNoContent || w.Header().Get("HX-Redirect") == "" {
		t.Fatalf("the unchanged save: HTTP %d %s, redirect %q", w.Code, w.Body.String(), w.Header().Get("HX-Redirect"))
	}
	if notified, recorded = sent.sent(); len(notified) != 0 || len(recorded) != 0 {
		t.Fatalf("a save that changed nothing recorded %v and notified %v", recorded, notified)
	}
	if ts, err := f.store.GetSpec(context.Background(), "spec-1"); err != nil || ts.Version() != 2 {
		t.Fatalf("the unchanged save moved the spec: %v %v", ts, err)
	}
}
