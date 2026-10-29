package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// notSecure is the cookie policy of a plain HTTP deployment.
func notSecure(*http.Request) bool { return false }

type failedVerifier struct{}

func (failedVerifier) Verify(context.Context, string) (*identity.Identity, error) {
	return nil, auth.ErrExpiredToken
}

func TestAuthEmptyRefreshCookiesNeverAuthorize(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized request reached handler") })
	for _, cookies := range []string{"access_token=expired; refresh_token=; session_id=s", "access_token=expired; refresh_token=r; session_id=", "refresh_token=; session_id="} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", cookies)
		w := httptest.NewRecorder()
		Auth(failedVerifier{}, nil, notSecure, zerolog.Nop())(next).ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("cookies %q status %d", cookies, w.Code)
		}
	}
}

func TestMalformedAuthorizationDoesNotFallbackToCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic bad")
	r.Header.Set("Cookie", "access_token=valid")
	raw, fromHeader := extractBearer(r)
	if raw != "" || !fromHeader {
		t.Fatalf("malformed header fell back: %q %v", raw, fromHeader)
	}
	if _, err := tryRefresh(httptest.NewRecorder(), r, nil, notSecure); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatal(err)
	}
}

// sessionOutage fails every session read, as an unreachable store would.
type sessionOutage struct{ storage.Storage }

func (sessionOutage) GetSession(context.Context, string) (*model.Session, error) {
	return nil, storage.ErrUnavailable
}

// When the credentials cannot be checked, Auth and OptionalAuth answer 503
// and log why at ERROR, with the method and the path, never a credential.
func TestAuthLogsCredentialsItCannotCheck(t *testing.T) {
	store := sessionOutage{inmemory.New()}
	svc := session.New(store, fixtureIssuer{}, token.RealClock(), session.Config{}, rbac.NewResolver(store), nil)
	verifier := fixtureVerifier{&identity.Identity{UserID: "u", SessionID: "session-secret"}}
	for _, tc := range []struct {
		name string
		mw   func(token.Verifier, *session.Service, func(*http.Request) bool, zerolog.Logger) func(http.Handler) http.Handler
	}{{"Auth", Auth}, {"OptionalAuth", OptionalAuth}} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("request reached the handler") })
			r := httptest.NewRequest(http.MethodPost, "/logout?next=1", nil)
			r.Header.Set("Cookie", "access_token=access-secret; refresh_token=refresh-secret; session_id=session-secret")
			w := httptest.NewRecorder()
			tc.mw(verifier, svc, notSecure, zerolog.New(&logs))(next).ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("HTTP %d, want 503", w.Code)
			}
			var entry struct {
				Level  string `json:"level"`
				Method string `json:"method"`
				Path   string `json:"path"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "error" || entry.Method != http.MethodPost ||
				entry.Path != "/logout" || entry.Error != storage.ErrUnavailable.Error() {
				t.Fatalf("log %q, want one ERROR line with the method, the path and the error", logs.String())
			}
			for _, secret := range []string{"access-secret", "refresh-secret", "session-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("log holds the credential %q: %s", secret, logs.String())
				}
			}
		})
	}
}
