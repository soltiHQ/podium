package session

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Revoke and Delete log no session ID: the session_id cookie carries it, and
// with the refresh token it renews the session.
func TestLogLinesLeaveTheSessionIDOut(t *testing.T) {
	const id = "secret-session-id"
	ctx := context.Background()
	owner := &identity.Identity{UserID: "owner"}
	for _, tc := range []struct {
		line string
		call func(*Service) error
	}{
		{"session revoked", func(s *Service) error { return s.Revoke(ctx, owner, RevokeRequest{ID: id}) }},
		{"session deleted", func(s *Service) error { return s.Delete(ctx, DeleteRequest{ID: id}) }},
	} {
		t.Run(tc.line, func(t *testing.T) {
			store := inmemory.New()
			sess, err := model.NewSession(id, owner.UserID, "cred-owner", enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.CreateSession(ctx, sess); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			if err := tc.call(New(store, zerolog.New(&logs).Level(zerolog.DebugLevel))); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if !strings.Contains(out, `"message":"`+tc.line+`"`) {
				t.Fatalf("no %q line: %s", tc.line, out)
			}
			if strings.Contains(out, id) || strings.Contains(out, "session_id") {
				t.Fatalf("the log carries the session ID: %s", out)
			}
		})
	}
}
