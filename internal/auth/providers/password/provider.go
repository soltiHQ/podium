package password

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Provider authenticates using (subject, password).
type Provider struct {
	store storage.Storage
	// dummyHash returns the bcrypt hash a login checks its password against
	// when there is no stored hash to check. It is generated on first use.
	dummyHash func() ([]byte, error)
}

// New creates a new password provider. Its dummy hash has
// credentials.DefaultBcryptCost, the cost passwords are stored with.
func New(store storage.Storage) *Provider {
	return newProvider(store, credentials.DefaultBcryptCost)
}

// newProvider is New with the bcrypt cost of the dummy hash.
func newProvider(store storage.Storage, cost int) *Provider {
	return &Provider{
		store: store,
		dummyHash: sync.OnceValues(func() ([]byte, error) {
			return bcrypt.GenerateFromPassword([]byte("no stored password to check"), cost)
		}),
	}
}

// Kind returns the provider kind.
func (*Provider) Kind() enum.Auth { return enum.Password }

// Authenticate authenticates the user by subject/password.
//
// Contract:
//   - Does not leak which field was wrong (subject/password/user state).
//   - Takes as long for an unknown or disabled subject, or one without a
//     password, as for a wrong password: each pays for one bcrypt compare.
//   - Uses Credential as (user <-> auth kind) binding.
//   - Uses Verifier as verification material store (bcrypt hash, params, etc).
//   - A password over credentials.MaxPasswordBytes is wrong: bcrypt would
//     check only its first 72 bytes.
//
// Errors:
//   - auth.ErrInvalidRequest for invalid wiring or wrong request type/kind.
//   - auth.ErrInvalidCredentials for subject/password mismatch (no field leakage).
//   - may return storage.ErrUnavailable / storage.ErrInternal from the backend.
func (p *Provider) Authenticate(ctx context.Context, req providers.Request) (*providers.Result, error) {
	if p == nil || p.store == nil {
		return nil, auth.ErrInvalidRequest
	}
	if req == nil || req.AuthKind() != enum.Password {
		return nil, auth.ErrInvalidRequest
	}

	r, ok := req.(Request)
	if !ok {
		return nil, auth.ErrInvalidRequest
	}
	if r.Subject == "" || r.Password == "" {
		return nil, auth.ErrInvalidCredentials
	}

	u, err := p.store.GetUserBySubject(ctx, r.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, p.invalid(r.Password)
		}
		return nil, err
	}
	if u.Disabled() {
		return nil, p.invalid(r.Password)
	}

	cred, err := p.store.GetCredentialByUserAndAuth(ctx, u.ID(), enum.Password)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, p.invalid(r.Password)
		}
		return nil, err
	}

	ver, err := p.store.GetVerifierByCredential(ctx, cred.ID())
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, p.invalid(r.Password)
		}
		return nil, err
	}
	if len(r.Password) > credentials.MaxPasswordBytes {
		return nil, p.invalid(r.Password)
	}
	if err = credentials.VerifyPassword(cred, ver, r.Password); err != nil {
		return nil, auth.ErrInvalidCredentials
	}

	return &providers.Result{
		User:       u,
		Credential: cred,
	}, nil
}

// invalid checks password against the dummy hash, ignoring the result, and
// returns auth.ErrInvalidCredentials, so a login that fails before a stored
// hash is checked takes as long as one with a wrong password.
func (p *Provider) invalid(password string) error {
	if hash, err := p.dummyHash(); err == nil {
		_ = bcrypt.CompareHashAndPassword(hash, []byte(password))
	}
	return auth.ErrInvalidCredentials
}
