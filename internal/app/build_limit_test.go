package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/cluster/standalone"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

// Behind a trusted proxy the main listener counts failures per forwarded client:
// a client blocked by failed logins or by 401 responses leaves the others alone.
func TestMainHandlerLimitsForwardedClientsSeparately(t *testing.T) {
	logger := zerolog.Nop()
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("limit-test-only-secret", 2)
	cfg.TrustedProxies = []string{"10.0.0.1"}
	store := inmemory.New()
	authModel := kit.New(store, cfg.Auth)
	svc, err := initServices(store, authModel, cfg.History.Archive, logger)
	if err != nil {
		t.Fatal(err)
	}
	pool := proxy.NewPool(nil, cfg.AgentAuth.Require)
	defer func() { _ = pool.Close() }()
	events := event.NewHub(logger)
	defer events.Close()
	resolver, err := clientip.New(cfg.TrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	handler, logs := buildMainHandler(cfg, logger, svc, authModel, pool, events, standalone.NewLeadership(), resolver)
	defer logs.Close()

	// do sends a request through the trusted proxy on behalf of client.
	do := func(method, path, client string, form url.Values) int {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.RemoteAddr = "10.0.0.1:443"
		r.Header.Set("X-Forwarded-For", client)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	login := func(client, subject string) int {
		return do(http.MethodPost, "/login", client, url.Values{"subject": {subject}, "password": {"wrong-password"}})
	}
	const blocked, other = "203.0.113.9", "198.51.100.7"

	for i := range 5 {
		if code := login(blocked, fmt.Sprintf("user-%d", i)); code != http.StatusSeeOther {
			t.Fatalf("failed login %d: HTTP %d, want 303", i+1, code)
		}
	}
	if code := login(blocked, "user-5"); code != http.StatusTooManyRequests {
		t.Fatalf("login from the blocked client: HTTP %d, want 429", code)
	}
	if code := login(other, "user-5"); code != http.StatusSeeOther {
		t.Fatalf("login from another client behind the proxy: HTTP %d, want 303", code)
	}

	for i := range 5 {
		if code := do(http.MethodGet, "/api/v1/users", blocked, nil); code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated call %d: HTTP %d, want 401", i+1, code)
		}
	}
	if code := do(http.MethodGet, "/login", blocked, nil); code != http.StatusTooManyRequests {
		t.Fatalf("blocked client: HTTP %d, want 429", code)
	}
	if code := do(http.MethodGet, "/login", other, nil); code != http.StatusOK {
		t.Fatalf("another client behind the proxy: HTTP %d, want 200", code)
	}
}
