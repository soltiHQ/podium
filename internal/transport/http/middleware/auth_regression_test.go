package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
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
		Auth(failedVerifier{}, nil, notSecure)(next).ServeHTTP(w, r)
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
