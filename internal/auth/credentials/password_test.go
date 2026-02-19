package credentials

import (
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
)

func TestNewPasswordCredential_CreatesCredential(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1")
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	if cred == nil {
		t.Fatalf("expected non-nil credential")
	}
	if cred.AuthKind() != kind.Password {
		t.Fatalf("expected kind.Password, got=%v", cred.AuthKind())
	}

	// Credential must not store any verification material.
	if _, ok := cred.Secret(PasswordHashKey); ok {
		t.Fatalf("credential must not contain password hash in secrets[%q]", PasswordHashKey)
	}
}

func TestNewPasswordVerifier_EmptyPassword(t *testing.T) {
	t.Parallel()

	_, err := NewPasswordVerifier("ver-1", "cred-1", "", DefaultBcryptCost)
	if !errors.Is(err, auth.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, err=%v", err)
	}
}

func TestNewPasswordVerifier_SetsHash(t *testing.T) {
	t.Parallel()

	v, err := NewPasswordVerifier("ver-1", "cred-1", "secret-password", 4)
	if err != nil {
		t.Fatalf("NewPasswordVerifier err=%v", err)
	}
	if v == nil {
		t.Fatalf("expected non-nil verifier")
	}
	if v.AuthKind() != kind.Password {
		t.Fatalf("expected kind.Password, got=%v", v.AuthKind())
	}
	if v.CredentialID() != "cred-1" {
		t.Fatalf("expected CredentialID=cred-1, got=%q", v.CredentialID())
	}

	hash, ok := v.DataGet(PasswordHashKey)
	if !ok || hash == "" {
		t.Fatalf("expected stored hash in data[%q]", PasswordHashKey)
	}
}

func TestVerifyPassword_SuccessAndMismatch(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1")
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	v, err := NewPasswordVerifier("ver-1", cred.ID(), "pw-1", 4)
	if err != nil {
		t.Fatalf("NewPasswordVerifier err=%v", err)
	}

	if err = VerifyPassword(cred, v, "pw-1"); err != nil {
		t.Fatalf("expected success, err=%v", err)
	}
	if err = VerifyPassword(cred, v, "wrong"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
}

func TestVerifyPassword_NilInputs(t *testing.T) {
	t.Parallel()

	cred, _ := NewPasswordCredential("cred-1", "user-1")
	v, _ := NewPasswordVerifier("ver-1", "cred-1", "pw", 4)

	if err := VerifyPassword(nil, v, "pw"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
	if err := VerifyPassword(cred, nil, "pw"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
	// empty plaintext treated as mismatch (avoid leaking details)
	if err := VerifyPassword(cred, v, ""); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
}

func TestVerifyPassword_WrongAuthKind(t *testing.T) {
	t.Parallel()

	cred, err := model.NewCredential("cred-1", "user-1", kind.APIKey)
	if err != nil {
		t.Fatalf("model.NewCredential err=%v", err)
	}
	v, err := model.NewVerifier("ver-1", "cred-1", kind.Password)
	if err != nil {
		t.Fatalf("model.NewVerifier err=%v", err)
	}

	if err = VerifyPassword(cred, v, "pw"); !errors.Is(err, auth.ErrWrongAuthKind) {
		t.Fatalf("expected ErrWrongAuthKind, err=%v", err)
	}
}

func TestVerifyPassword_CredentialIDMismatch(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1")
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	v, err := NewPasswordVerifier("ver-1", "cred-OTHER", "pw", 4)
	if err != nil {
		t.Fatalf("NewPasswordVerifier err=%v", err)
	}

	if err = VerifyPassword(cred, v, "pw"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, err=%v", err)
	}
}

func TestVerifyPassword_MissingHash(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1")
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	v, err := model.NewVerifier("ver-1", cred.ID(), kind.Password)
	if err != nil {
		t.Fatalf("model.NewVerifier err=%v", err)
	}

	if err = VerifyPassword(cred, v, "pw"); !errors.Is(err, auth.ErrMissingPasswordHash) {
		t.Fatalf("expected ErrMissingPasswordHash, err=%v", err)
	}
}

func TestVerifyPassword_CorruptedHash(t *testing.T) {
	t.Parallel()

	cred, err := NewPasswordCredential("cred-1", "user-1")
	if err != nil {
		t.Fatalf("NewPasswordCredential err=%v", err)
	}
	v, err := model.NewVerifier("ver-1", cred.ID(), kind.Password)
	if err != nil {
		t.Fatalf("model.NewVerifier err=%v", err)
	}

	// Corrupted verifier payload.
	if err = v.DataSet(PasswordHashKey, "not-a-bcrypt-hash"); err != nil {
		t.Fatalf("DataSet err=%v", err)
	}

	if err = VerifyPassword(cred, v, "pw"); !errors.Is(err, auth.ErrMissingPasswordHash) {
		t.Fatalf("expected ErrMissingPasswordHash for corrupted hash, err=%v", err)
	}
}
