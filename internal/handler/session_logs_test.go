package handler

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// failedTx is a store whose transactions fail as an unreachable store's do.
type failedTx struct{ storage.Storage }

func (failedTx) WithTx(context.Context, func(storage.Storage) error) error {
	return storage.ErrUnavailable
}

// A session revoke logs no session ID, at the handler or in the session
// service, whether it succeeds or fails: the session_id cookie carries the ID,
// and with the refresh token it renews the session.
func TestSessionRevokeLogsNoSessionID(t *testing.T) {
	const id = "secret-session-id"
	for _, tc := range []struct {
		name   string
		wrap   func(storage.Storage) storage.Storage
		status int
		line   string
		lines  int // the handler's line, and the session service's on success
	}{
		{"revoked", nil, http.StatusNoContent, "session revoked", 2},
		{"store down", func(s storage.Storage) storage.Storage { return failedTx{s} }, http.StatusServiceUnavailable, "session revoke failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			f := newAPIFixtureLogging(t, tc.wrap, zerolog.New(&logs).Level(zerolog.DebugLevel))
			me := f.user(t, "me", enum.RoleUserAdminID)
			f.session(t, id, "me")

			if w := f.do(http.MethodPost, routepath.ApiUserRevokeSession(id), "", me); w.Code != tc.status {
				t.Fatalf("revoke: %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			out := logs.String()
			if got := strings.Count(out, `"message":"`+tc.line+`"`); got != tc.lines {
				t.Fatalf("%d %q lines, want %d: %s", got, tc.line, tc.lines, out)
			}
			if strings.Contains(out, id) || strings.Contains(out, "session_id") {
				t.Fatalf("the log carries the session ID: %s", out)
			}
		})
	}
}
