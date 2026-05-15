package password

import (
	"context"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type badKindReq struct{}

func (badKindReq) AuthKind() enum.Auth { return enum.APIKey }

type wrongTypeReq struct{}

func (wrongTypeReq) AuthKind() enum.Auth { return enum.Password }

type wrapStore struct {
	storage.Storage

	getUserErr     error
	getCredErr     error
	getVerifierErr error
}

func (w wrapStore) GetUserBySubject(ctx context.Context, subject string) (*model.User, error) {
	if w.getUserErr != nil {
		return nil, w.getUserErr
	}
	return w.Storage.GetUserBySubject(ctx, subject)
}

func (w wrapStore) GetCredentialByUserAndAuth(ctx context.Context, userID string, authKind enum.Auth) (*model.Credential, error) {
	if w.getCredErr != nil {
		return nil, w.getCredErr
	}
	return w.Storage.GetCredentialByUserAndAuth(ctx, userID, authKind)
}

func (w wrapStore) GetVerifierByCredential(ctx context.Context, credentialID string) (*model.Verifier, error) {
	if w.getVerifierErr != nil {
		return nil, w.getVerifierErr
	}
	return w.Storage.GetVerifierByCredential(ctx, credentialID)
}

func mustUser(t *testing.T, id, subject string, disabled bool) *model.User {
	t.Helper()

	u, err := model.NewUser(id, subject)
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	if disabled {
		u.Disable()
	}
	return u
}

func mustUpsertUser(t *testing.T, ctx context.Context, store storage.Storage, u *model.User) {
	t.Helper()

	if err := store.UpsertUser(ctx, u); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
}

func mustUpsertPasswordMaterial(t *testing.T, ctx context.Context, store storage.Storage, credID, verID, userID, plain string) (*model.Credential, *model.Verifier) {
	t.Helper()

	cred, err := credentials.NewPasswordCredential(credID, userID)
	if err != nil {
		t.Fatalf("NewPasswordCredential: %v", err)
	}
	ver, err := credentials.NewPasswordVerifier(verID, credID, plain, credentials.DefaultBcryptCost)
	if err != nil {
		t.Fatalf("NewPasswordVerifier: %v", err)
	}
	if err := store.UpsertCredential(ctx, cred); err != nil {
		t.Fatalf("UpsertCredential: %v", err)
	}
	if err := store.UpsertVerifier(ctx, ver); err != nil {
		t.Fatalf("UpsertVerifier: %v", err)
	}
	return cred, ver
}

func TestProvider_Authenticate_InvalidWiring(t *testing.T) {
	ctx := context.Background()

	t.Run("nil provider", func(t *testing.T) {
		var p *Provider
		_, err := p.Authenticate(ctx, Request{Subject: "s", Password: "p"})
		if !errors.Is(err, auth.ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
	})

	t.Run("nil store", func(t *testing.T) {
		p := &Provider{store: nil}
		_, err := p.Authenticate(ctx, Request{Subject: "s", Password: "p"})
		if !errors.Is(err, auth.ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
	})
}

func TestProvider_Authenticate_InvalidRequest(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	t.Run("nil req", func(t *testing.T) {
		_, err := p.Authenticate(ctx, nil)
		if !errors.Is(err, auth.ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
	})

	t.Run("wrong auth kind", func(t *testing.T) {
		_, err := p.Authenticate(ctx, badKindReq{})
		if !errors.Is(err, auth.ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
	})

	t.Run("correct kind but wrong concrete type", func(t *testing.T) {
		_, err := p.Authenticate(ctx, wrongTypeReq{})
		if !errors.Is(err, auth.ErrInvalidRequest) {
			t.Fatalf("expected ErrInvalidRequest, got %v", err)
		}
	})
}

func TestProvider_Authenticate_InvalidCredentials_Shape(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	t.Run("empty subject", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Request{Subject: "", Password: "x"})
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("expected ErrInvalidCredentials, got %v", err)
		}
	})

	t.Run("empty password", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Request{Subject: "x", Password: ""})
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("expected ErrInvalidCredentials, got %v", err)
		}
	})
}

func TestProvider_Authenticate_UserNotFound(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	_, err := p.Authenticate(ctx, Request{Subject: "subj-404", Password: "pw"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestProvider_Authenticate_UserDisabled(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	u := mustUser(t, "user-1", "subj-1", true)
	mustUpsertUser(t, ctx, store, u)

	_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestProvider_Authenticate_CredentialNotFound(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	u := mustUser(t, "user-1", "subj-1", false)
	mustUpsertUser(t, ctx, store, u)

	_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestProvider_Authenticate_VerifierNotFound(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	u := mustUser(t, "user-1", "subj-1", false)
	mustUpsertUser(t, ctx, store, u)

	// Create only credential (no verifier).
	cred, err := credentials.NewPasswordCredential("cred-1", u.ID())
	if err != nil {
		t.Fatalf("NewPasswordCredential: %v", err)
	}
	if err := store.UpsertCredential(ctx, cred); err != nil {
		t.Fatalf("UpsertCredential: %v", err)
	}

	_, err = p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestProvider_Authenticate_BadPassword(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	u := mustUser(t, "user-1", "subj-1", false)
	mustUpsertUser(t, ctx, store, u)
	_, _ = mustUpsertPasswordMaterial(t, ctx, store, "cred-1", "ver-1", u.ID(), "correct-password")

	_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "wrong-password"})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestProvider_Authenticate_Success(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	p := New(store)

	u := mustUser(t, "user-1", "subj-1", false)
	mustUpsertUser(t, ctx, store, u)
	cred, _ := mustUpsertPasswordMaterial(t, ctx, store, "cred-1", "ver-1", u.ID(), "pw")

	res, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res == nil || res.User == nil || res.Credential == nil {
		t.Fatalf("expected non-nil result with user+credential, got %#v", res)
	}
	if res.User.ID() != u.ID() {
		t.Fatalf("expected user id %q, got %q", u.ID(), res.User.ID())
	}
	if res.Credential.ID() != cred.ID() {
		t.Fatalf("expected cred id %q, got %q", cred.ID(), res.Credential.ID())
	}
	if res.Credential.AuthKind() != enum.Password {
		t.Fatalf("expected auth kind %q, got %q", enum.Password, res.Credential.AuthKind())
	}
}

func TestProvider_Authenticate_PropagatesStorageErrors(t *testing.T) {
	ctx := context.Background()
	base := inmemory.New()

	t.Run("GetUserBySubject returns ErrUnavailable", func(t *testing.T) {
		p := New(wrapStore{Storage: base, getUserErr: storage.ErrUnavailable})
		_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
		if !errors.Is(err, storage.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable, got %v", err)
		}
	})

	t.Run("GetCredentialByUserAndAuth returns ErrInternal", func(t *testing.T) {
		u := mustUser(t, "user-1", "subj-1", false)
		if err := base.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser: %v", err)
		}

		p := New(wrapStore{Storage: base, getCredErr: storage.ErrInternal})
		_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
		if !errors.Is(err, storage.ErrInternal) {
			t.Fatalf("expected ErrInternal, got %v", err)
		}
	})

	t.Run("GetVerifierByCredential returns ErrUnavailable", func(t *testing.T) {
		u := mustUser(t, "user-1", "subj-1", false)
		if err := base.UpsertUser(ctx, u); err != nil {
			t.Fatalf("UpsertUser: %v", err)
		}
		_, _ = mustUpsertPasswordMaterial(t, ctx, base, "cred-1", "ver-1", u.ID(), "pw")

		p := New(wrapStore{Storage: base, getVerifierErr: storage.ErrUnavailable})
		_, err := p.Authenticate(ctx, Request{Subject: "subj-1", Password: "pw"})
		if !errors.Is(err, storage.ErrUnavailable) {
			t.Fatalf("expected ErrUnavailable, got %v", err)
		}
	})
}
