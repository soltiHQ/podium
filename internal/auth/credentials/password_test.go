package credentials

import (
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
)

func TestNewPasswordCredential_EmptyPassword(t *testing.T) {
	t.Parallel()

	_, err := NewPasswordCredential("cred-1", "user-1", "", DefaultBcryptCost)
	if !errors.Is(err, auth.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, err=%v", err)
	}
}

func TestNewPasswordCredential_SetsHash(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1", "secret-password", 4)
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	if cred == nil {
		t.Fatalf("expected non-nil credential")
	}
	if cred.AuthKind() != kind.Password {
		t.Fatalf("expected kind.Password, got=%v", cred.AuthKind())
	}

	hash, ok := cred.Secret(PasswordHashKey)
	if !ok || hash == "" {
		t.Fatalf("expected stored hash in secret[%q]", PasswordHashKey)
	}
}

func TestVerifyPassword_SuccessAndMismatch(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1", "pw-1", 4)
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	if err = VerifyPassword(cred, "pw-1"); err != nil {
		t.Fatalf("expected success, err=%v", err)
	}
	if err = VerifyPassword(cred, "wrong"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
}

func TestVerifyPassword_NilCredential(t *testing.T) {
	t.Parallel()

	if err := VerifyPassword(nil, "pw"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
}

func TestVerifyPassword_WrongAuthKind(t *testing.T) {
	t.Parallel()

	cred, err := model.NewCredential("cred-1", "user-1", kind.APIKey)
	if err != nil {
		t.Fatalf("model.NewCredential err=%v", err)
	}
	if err = VerifyPassword(cred, "pw"); !errors.Is(err, auth.ErrWrongAuthKind) {
		t.Fatalf("expected ErrWrongAuthKind, err=%v", err)
	}
}

func TestVerifyPassword_MissingHash(t *testing.T) {
	t.Parallel()

	cred, err := model.NewCredential("cred-1", "user-1", kind.Password)
	if err != nil {
		t.Fatalf("model.NewCredential err=%v", err)
	}
	if err = VerifyPassword(cred, "pw"); !errors.Is(err, auth.ErrMissingPasswordHash) {
		t.Fatalf("expected ErrMissingPasswordHash, err=%v", err)
	}
}

func TestVerifyPassword_CorruptedHash(t *testing.T) {
	t.Parallel()

	cred, err := model.NewCredential("cred-1", "user-1", kind.Password)
	if err != nil {
		t.Fatalf("model.NewCredential err=%v", err)
	}
	if err = cred.SetSecret(PasswordHashKey, "not-a-bcrypt-hash"); err != nil {
		t.Fatalf("cred.SetSecret err=%v", err)
	}
	if err = VerifyPassword(cred, "pw"); !errors.Is(err, auth.ErrMissingPasswordHash) {
		t.Fatalf("expected ErrMissingPasswordHash for corrupted hash, err=%v", err)
	}
}
