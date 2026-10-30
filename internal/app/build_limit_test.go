package app

import (
	"fmt"
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
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

// mainHandlerSecret signs the access tokens of the listeners mainHandlerOver builds.
const mainHandlerSecret = "main-handler-test-only-signing-secret"

// mainHandler builds the main listener from cfg over an in-memory store.
func mainHandler(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	return mainHandlerOver(t, cfg, inmemory.New(), zerolog.Nop())
}

// mainHandlerOver builds the main listener from cfg over store, logging to logger.
func mainHandlerOver(t *testing.T, cfg config.Config, store storage.Storage, logger zerolog.Logger) http.Handler {
	t.Helper()
	cfg.Auth.JWTSecret = mainHandlerSecret
	authModel := kit.New(store, cfg.Auth)
	svc, err := initServices(store, authModel, cfg.History.Archive, cfg.AgentAuth, logger)
	if err != nil {
		t.Fatal(err)
	}
	pool := proxy.NewPool(nil, cfg.AgentAuth.Require)
	t.Cleanup(func() { _ = pool.Close() })
	events := event.NewHub(logger)
	t.Cleanup(events.Close)
	resolver, err := clientip.New(cfg.TrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	handler, logs := buildMainHandler(cfg, logger, svc, authModel, pool, events, standalone.NewLeadership(), resolver)
	t.Cleanup(logs.Close)
	return handler
}

// The main listener has no limit on failed authentication: its credentials
// (JWTs, session IDs, refresh tokens) cannot be guessed, and passwords go
// through the login limiter.
func TestMainListenerNeverLimitsAuthFailures(t *testing.T) {
	handler := mainHandler(t, config.Default())
	get := func(path string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	for i := range 10 {
		if code := get("/api/v1/users"); code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated call %d: HTTP %d, want 401", i+1, code)
		}
	}
	if code := get("/login"); code != http.StatusOK {
		t.Fatalf("GET /login after ten 401 responses: HTTP %d, want 200", code)
	}
}

// Behind a trusted proxy, failed logins count per forwarded client: a client
// blocked by failed logins leaves the others alone.
func TestMainHandlerLimitsForwardedClientsSeparately(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxies = []string{"10.0.0.1"}
	handler := mainHandler(t, cfg)

	// login sends a failing login through the trusted proxy on behalf of client.
	login := func(client, subject string) int {
		form := url.Values{"subject": {subject}, "password": {"wrong-password"}}
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "10.0.0.1:443"
		r.Header.Set("X-Forwarded-For", client)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
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
}
