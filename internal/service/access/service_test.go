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
	"github.com/soltiHQ/control-plane/domain/model"
	iauth "github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/devicecookie"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/storage"
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
		Devices: devicecookie.NewSigner([]byte(cfg.JWTSecret)),
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

// outage is a store whose user lookups fail while down is set.
type outage struct {
	storage.Storage
	down atomic.Bool
}

func (s *outage) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	if s.down.Load() {
		return nil, storage.ErrUnavailable
	}
	return s.Storage.GetUserBySubject(ctx, subject)
}

// Only a wrong subject or password counts toward the limit. Any other failure,
// such as a storage outage or a user without permissions, releases the
// attempt: it never blocks the account or the client IP.
func TestLoginCountsOnlyInvalidCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		down     bool
		want     error
	}{
		{"storage outage", "wrong-password", true, storage.ErrUnavailable},
		{"user without permissions", "nobody-password", false, iauth.ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := &outage{Storage: inmemory.New()}
			u, err := model.NewUser("user-nobody", "nobody")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			if err := credential.New(store, zerolog.Nop()).SetPassword(ctx, credential.SetPasswordRequest{UserID: u.ID(), Password: "nobody-password", Cost: 4}); err != nil {
				t.Fatal(err)
			}
			cfg := iauth.Config{JWTSecret: strings.Repeat("k", iauth.MinJWTSecretLen)}.WithDefaults()
			svc := New(kit.New(store, cfg), store, zerolog.Nop())
			login := func(password string) error {
				_, _, err := svc.Login(ctx, LoginRequest{Subject: "nobody", Password: password, ClientIP: "203.0.113.9"})
				return err
			}

			store.down.Store(tc.down)
			for i := range 2 * cfg.RateAttempts {
				if err := login(tc.password); !errors.Is(err, tc.want) || errors.Is(err, iauth.ErrRateLimited) {
					t.Fatalf("attempt %d: %v, want %v", i+1, err, tc.want)
				}
			}
			store.down.Store(false)
			if err := login("wrong-password"); !errors.Is(err, iauth.ErrInvalidCredentials) {
				t.Fatalf("wrong password afterwards: %v, want ErrInvalidCredentials, not a blocked key", err)
			}
		})
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
