package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/buildinfo"
)

// probe is a handler whose readiness is ready ("" for ready) and whose /leaderz
// reason is leader ("" for the leader).
func probe(ready, leader string) *Handler {
	return NewHandler(Probes{
		Ready:  func() string { return ready },
		Leader: func() string { return leader },
		Version: buildinfo.Info{
			Version: "v1.2.3", Commit: "0123456789abcdef", GoVersion: "go1.27.3", Platform: "linux/amd64",
			BuildTime: time.Date(2026, 11, 5, 11, 35, 33, 0, time.UTC),
		},
	})
}

// reply is the answer to a request.
type reply struct {
	Code   int
	Header http.Header
	Body   *bytes.Buffer
}

// get serves a request to h over a real connection, which drops the body of a
// HEAD answer as a server does.
func get(t *testing.T, h http.Handler, method, path string) reply {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	if _, err = body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header, &body}
}

// Each route answers GET and HEAD with its status, and a line of text for the
// probes; any other method is refused, and an unknown path is not found.
func TestRoutesAnswerGetAndHead(t *testing.T) {
	for _, tc := range []struct {
		name, ready, leader, path string
		status                    int
		body                      string
	}{
		{"live", "starting", "starting", "/livez", 200, "ok\n"},
		{"ready", "", "not leader", "/readyz", 200, "ok\n"},
		{"starting", "starting", "", "/readyz", 503, "starting\n"},
		{"storage", "storage failed", "", "/readyz", 503, "storage failed\n"},
		{"no leader", "no raft leader", "not leader", "/readyz", 503, "no raft leader\n"},
		{"shutting down", "shutting down", "", "/readyz", 503, "shutting down\n"},
		{"leader", "", "", "/leaderz", 200, "ok\n"},
		{"follower", "", "not leader", "/leaderz", 503, "not leader\n"},
		{"leader during the shutdown delay", "shutting down", "", "/leaderz", 200, "ok\n"},
		{"not leader while starting", "starting", "starting", "/leaderz", 503, "starting\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := probe(tc.ready, tc.leader)
			w := get(t, h, http.MethodGet, tc.path)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("GET %s: %d %q, want %d %q", tc.path, w.Code, w.Body.String(), tc.status, tc.body)
			}
			if got := w.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Fatalf("Content-Type %q, want text/plain", got)
			}
			head := get(t, h, http.MethodHead, tc.path)
			if head.Code != tc.status || head.Body.Len() != 0 {
				t.Fatalf("HEAD %s: %d with %d body bytes, want %d and none", tc.path, head.Code, head.Body.Len(), tc.status)
			}
		})
	}
	h := probe("", "")
	if w := get(t, h, http.MethodPost, "/livez"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /livez: %d, want 405", w.Code)
	}
	if w := get(t, h, http.MethodGet, "/nowhere"); w.Code != http.StatusNotFound {
		t.Fatalf("GET /nowhere: %d, want 404", w.Code)
	}
}

// /version is the build information as JSON, whatever the readiness.
func TestVersionIsBuildInfoJSON(t *testing.T) {
	w := get(t, probe("starting", "starting"), http.MethodGet, "/version")
	if w.Code != http.StatusOK || w.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /version: %d %q", w.Code, w.Header.Get("Content-Type"))
	}
	var got buildinfo.Info
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := buildinfo.Info{
		Version: "v1.2.3", Commit: "0123456789abcdef", GoVersion: "go1.27.3", Platform: "linux/amd64",
		BuildTime: time.Date(2026, 11, 5, 11, 35, 33, 0, time.UTC),
	}
	if got != want {
		t.Fatalf("version %+v, want %+v", got, want)
	}
	if !strings.Contains(w.Body.String(), `"go_version":"go1.27.3"`) {
		t.Fatalf("body %q names no go_version", w.Body.String())
	}
}

// Handle adds a route to the listener: what the metrics route is registered with.
func TestHandleAddsARoute(t *testing.T) {
	h := probe("", "")
	h.Handle("GET /metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("metrics"))
	}))
	if w := get(t, h, http.MethodGet, "/metrics"); w.Code != 200 || w.Body.String() != "metrics" {
		t.Fatalf("GET /metrics: %d %q", w.Code, w.Body.String())
	}
}

// The listener is off when ops.addr is empty, and a malformed address is refused.
func TestConfig(t *testing.T) {
	if (Config{}).Enabled() || (Config{}).Validate() != nil {
		t.Fatal("an empty address must disable the listener without an error")
	}
	if !(Config{Addr: "127.0.0.1:8081"}).Enabled() {
		t.Fatal("an address must enable the listener")
	}
	if err := (Config{Addr: "8081"}).Validate(); err == nil {
		t.Fatal("an address without a host and port was accepted")
	}
	if got := (Config{Addr: ":1"}).HTTP().Name; got != "ops" {
		t.Fatalf("default name %q, want ops: another name would collide with the http listener's", got)
	}
}
