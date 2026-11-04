package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// followerStore is the store of a raft follower: its reads work and every
// transaction fails, as raft.Store.WithTx does.
type followerStore struct{ storage.Storage }

func (followerStore) WithTx(context.Context, func(storage.Storage) error) error {
	return fmt.Errorf("raft: WithTx: %w", cluster.ErrNotLeader)
}

// With auth.rotate_refresh a refresh rotates the refresh token, which only the
// leader can write. A browser whose access token has expired and whose request
// reaches a follower gets 503, with the leader's address in X-Leader when the
// follower knows it, and no cookie; the follower logs one WARN, no ERROR, and
// never a credential. Auth and OptionalAuth alike.
func TestRefreshOnAFollowerAsksForTheLeader(t *testing.T) {
	st, _, cookies := cookieRefreshFixture(t)
	clk := token.RealClock()
	secret := []byte("test-only-local-refresh-cookie-signing-key")
	svc := session.New(followerStore{st}, jwt.NewHSIssuer(secret, clk), clk, session.Config{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, Issuer: "cookie-test", Audience: "cookie-client", RotateRefresh: true}, rbac.NewResolver(st), nil)
	verifier := jwt.NewHSVerifier("cookie-test", "cookie-client", secret, clk)

	for _, mw := range []struct {
		name string
		wrap func(token.Verifier, *session.Service, func(*http.Request) bool, zerolog.Logger) func(http.Handler) http.Handler
	}{{"Auth", Auth}, {"OptionalAuth", OptionalAuth}} {
		for _, tc := range []struct {
			name       string
			leadership stubLeadership
			want       string // X-Leader
		}{
			{"leader known", stubLeadership{leader: "10.0.0.7:7000"}, "10.0.0.7:8080"},
			{"no leader known", stubLeadership{}, ""},
		} {
			t.Run(mw.name+", "+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request reached the handler") })
				handler := Leader(tc.leadership, LeaderOptions{ForwardPort: 8080})(mw.wrap(verifier, svc, notSecure, zerolog.New(&logs))(next))
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, cookieRefreshRequest(t.Context(), "/ui/specs?q=1", cookies))

				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("HTTP %d, want 503", w.Code)
				}
				if got := w.Header().Get("X-Leader"); got != tc.want {
					t.Fatalf("X-Leader = %q, want %q", got, tc.want)
				}
				if set := w.Result().Cookies(); len(set) != 0 {
					t.Fatalf("the answer sets cookies: %v", set)
				}
				var entry struct {
					Level   string `json:"level"`
					Method  string `json:"method"`
					Path    string `json:"path"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "warn" || entry.Method != http.MethodGet ||
					entry.Path != "/ui/specs" || !strings.Contains(entry.Message, "auth.rotate_refresh") {
					t.Fatalf("log %q, want one WARN line with the method, the path and auth.rotate_refresh", logs.String())
				}
				for _, secret := range []string{"test-only-original-refresh", "cookie-session"} {
					if strings.Contains(logs.String(), secret) {
						t.Fatalf("log holds the credential %q: %s", secret, logs.String())
					}
				}
			})
		}
	}
}
