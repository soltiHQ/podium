package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// writesFail is the store of a node whose writes fail: its reads work and every
// transaction fails with err, as raft.Store.WithTx does on a follower.
type writesFail struct {
	storage.Storage
	err error
}

func (s writesFail) WithTx(context.Context, func(storage.Storage) error) error { return s.err }

// A write that reached a follower, or lost leadership as the request ran, is a
// routing condition: the write handlers answer it with 503 and the leader's
// address in X-Leader, and log a WARN, not an ERROR. Any other failure of the
// store is answered as before, 503 without the header, and logged as an ERROR.
func TestWriteHandlersAnswerLeadershipErrorsWithTheLeader(t *testing.T) {
	const leader = "10.0.0.7:8080"
	for _, failure := range []struct {
		name   string
		err    error
		routed bool // whether it is a leadership error
	}{
		{"not the leader", fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader), true},
		{"leadership lost", fmt.Errorf("raft: apply: %w", cluster.ErrLeadershipLost), true},
		{"another failure", errors.New("disk on fire"), false},
	} {
		for _, tc := range []struct {
			name, method, target, body string
		}{
			{"user update", http.MethodPut, "/api/v1/users/reader", `{"name":"Reader"}`},
			{"user delete", http.MethodDelete, "/api/v1/users/reader", ""},
			{"user disable", http.MethodPost, "/api/v1/users/reader/disable", ""},
			{"password change", http.MethodPost, "/api/v1/users/reader/password", `{"password":"a-password-long-enough"}`},
			{"session revoke", http.MethodPost, "/api/v1/session/reader-session/revoke", ""},
			{"spec update", http.MethodPut, "/api/v1/specs/saved", `{"name":"Renamed","version":1,` + validKind + `}`},
			{"spec delete", http.MethodDelete, "/api/v1/specs/saved", ""},
			{"spec force-delete", http.MethodDelete, "/api/v1/specs/saved/force", ""},
		} {
			t.Run(failure.name+", "+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				f := newAPIFixtureLogging(t, func(s storage.Storage) storage.Storage { return writesFail{Storage: s, err: failure.err} }, zerolog.New(&logs))
				admin := f.user(t, "admin", enum.RoleAdminID)
				f.user(t, "reader", enum.RoleUserReaderID)
				saved, err := model.NewSpec("saved", "Saved spec", "lane")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.store.UpsertSpec(context.Background(), saved); err != nil {
					t.Fatal(err)
				}
				logs.Reset()

				r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
				if tc.body != "" {
					r.Header.Set("Content-Type", "application/json")
				}
				// The Leader middleware tells the handlers where the leader serves the listener.
				ctx := httpctx.WithLeader(transportctx.WithIdentity(r.Context(), admin), func() string { return leader })
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, r.WithContext(ctx))

				wantLeader := ""
				if failure.routed {
					wantLeader = leader
				}
				if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Leader") != wantLeader {
					t.Fatalf("HTTP %d with X-Leader %q, want 503 with %q: %s", w.Code, w.Header().Get("X-Leader"), wantLeader, w.Body.String())
				}
				wantLevel := "error"
				if failure.routed {
					wantLevel = "warn"
				}
				var failed []string // the levels of the lines of the handler about the failed write
				for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var line struct {
						Level   string `json:"level"`
						Handler string `json:"handler"`
						Message string `json:"message"`
					}
					if err := json.Unmarshal([]byte(raw), &line); err != nil {
						t.Fatalf("log line %q: %v", raw, err)
					}
					if line.Handler == "api" && strings.HasSuffix(line.Message, " failed") {
						failed = append(failed, line.Level)
					}
				}
				if len(failed) != 1 || failed[0] != wantLevel {
					t.Fatalf("the handler logged %v for the failed write, want one %s: %s", failed, wantLevel, logs.String())
				}
			})
		}
	}
}
