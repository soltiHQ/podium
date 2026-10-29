package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	iauth "github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// gatedPassword is a password provider that holds every check until release
// is closed, then fails it.
type gatedPassword struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (*gatedPassword) Kind() enum.Auth { return enum.Password }

func (g *gatedPassword) Authenticate(context.Context, providers.Request) (*providers.Result, error) {
	g.calls.Add(1)
	g.entered <- struct{}{}
	<-g.release
	return nil, iauth.ErrInvalidCredentials
}

func TestLoginLimiterBoundsConcurrentAttempts(t *testing.T) {
	const attempts = 20
	cfg := iauth.Config{JWTSecret: strings.Repeat("k", iauth.MinJWTSecretLen)}.WithDefaults()
	gate := &gatedPassword{entered: make(chan struct{}, attempts), release: make(chan struct{})}
	store := inmemory.New()
	clock := token.RealClock()
	svc := New(&kit.Auth{
		Clock:   clock,
		Limiter: kit.NewLimiter(cfg),
		Session: session.New(store, jwt.NewHSIssuer([]byte(cfg.JWTSecret), clock), clock, session.Config{}, rbac.NewResolver(store),
			map[enum.Auth]providers.Provider{enum.Password: gate}),
	}, store, zerolog.Nop())
	login := func(clientIP string) error {
		_, _, err := svc.Login(context.Background(), LoginRequest{Subject: "alice", Password: "wrong-password", ClientIP: clientIP})
		return err
	}

	// One account, every attempt from its own client IP: only the account key can refuse them.
	results := make(chan error, attempts)
	for i := range attempts {
		go func() { results <- login(fmt.Sprintf("203.0.113.%d", i+1)) }()
	}
	// Each attempt either waits inside the password check or comes back refused.
	admitted := 0
	for range attempts {
		select {
		case <-gate.entered:
			admitted++
		case err := <-results:
			if !errors.Is(err, iauth.ErrRateLimited) {
				t.Errorf("attempt that skipped the password check: %v, want ErrRateLimited", err)
			}
		}
	}
	if admitted > cfg.RateAttempts {
		t.Errorf("%d concurrent attempts reached the password check, want at most %d", admitted, cfg.RateAttempts)
	}

	close(gate.release)
	reached := 0
	for range admitted {
		err := <-results
		if !errors.Is(err, iauth.ErrInvalidCredentials) {
			t.Errorf("failed password check: %v, want ErrInvalidCredentials", err)
		}
		if errors.Is(err, iauth.ErrRateLimitReached) {
			reached++
		}
	}
	if reached != 1 {
		t.Errorf("%d failures reported blocking the account, want exactly 1", reached)
	}

	// The account is blocked: an attempt from a new client IP never reaches the password check.
	calls := gate.calls.Load()
	if err := login("198.51.100.7"); !errors.Is(err, iauth.ErrRateLimited) {
		t.Fatalf("attempt on the blocked account: %v, want ErrRateLimited", err)
	}
	if gate.calls.Load() != calls {
		t.Fatal("an attempt on the blocked account reached the password check")
	}
}

// A blank subject or password is an invalid request: it never reaches the
// limiter, where a whitespace-only subject would land on the shared
// "login:acct:" key.
func TestLoginRejectsBlankSubjectOrPassword(t *testing.T) {
	store := inmemory.New()
	svc := New(kit.New(store, iauth.Config{JWTSecret: strings.Repeat("k", iauth.MinJWTSecretLen)}), store, zerolog.Nop())
	for _, req := range []LoginRequest{
		{Subject: "", Password: "password", ClientIP: "203.0.113.9"},
		{Subject: " \t\n", Password: "password", ClientIP: "203.0.113.9"},
		{Subject: "alice", Password: "", ClientIP: "203.0.113.9"},
	} {
		if _, _, err := svc.Login(context.Background(), req); !errors.Is(err, iauth.ErrInvalidRequest) {
			t.Errorf("Login(%q, %q): %v, want ErrInvalidRequest", req.Subject, req.Password, err)
		}
	}
}
