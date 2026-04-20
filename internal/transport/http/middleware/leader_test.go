package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
)

// fakeLeadership for middleware tests.
type fakeLeadership struct {
	leader bool
	addr   string
}

func (f *fakeLeadership) AmLeader() bool        { return f.leader }
func (f *fakeLeadership) CurrentLeader() string { return f.addr }
func (f *fakeLeadership) WhenLeader(ctx context.Context, fn func(context.Context) error) error {
	if !f.leader {
		<-ctx.Done()
		return nil
	}
	return fn(ctx)
}

func okHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
}

func TestLeader_ReadPassesThroughOnFollower(t *testing.T) {
	mw := middleware.Leader(&fakeLeadership{leader: false}, middleware.LeaderOptions{})
	srv := mw(okHandler(http.StatusOK))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d", rec.Code)
	}
}

func TestLeader_WritePassesThroughOnLeader(t *testing.T) {
	mw := middleware.Leader(&fakeLeadership{leader: true}, middleware.LeaderOptions{})
	srv := mw(okHandler(http.StatusCreated))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 got %d", rec.Code)
	}
}

func TestLeader_WriteOnFollowerReturns503WithHeader(t *testing.T) {
	mw := middleware.Leader(&fakeLeadership{leader: false, addr: "leader.local:9090"}, middleware.LeaderOptions{})
	srv := mw(okHandler(http.StatusOK))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 got %d", rec.Code)
	}
	if rec.Header().Get("X-Leader") != "leader.local:9090" {
		t.Fatalf("X-Leader header missing: %q", rec.Header().Get("X-Leader"))
	}
}

func TestLeader_CustomIsWrite(t *testing.T) {
	mw := middleware.Leader(&fakeLeadership{leader: false}, middleware.LeaderOptions{
		IsWrite: func(r *http.Request) bool { return r.URL.Path == "/guarded" },
	})
	srv := mw(okHandler(http.StatusOK))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/open", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("open POST: want 200 got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/guarded", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("guarded POST: want 503 got %d", rec.Code)
	}
}
