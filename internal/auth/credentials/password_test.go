package credentials

import (
	"errors"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
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
	if cred.AuthKind() != enum.Password {
		t.Fatalf("expected enum.Password, got=%v", cred.AuthKind())
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
	if v.AuthKind() != enum.Password {
		t.Fatalf("expected enum.Password, got=%v", v.AuthKind())
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

	cred, err := model.NewCredential("cred-1", "user-1", enum.APIKey)
	if err != nil {
		t.Fatalf("model.NewCredential err=%v", err)
	}
	v, err := model.NewVerifier("ver-1", "cred-1", enum.Password)
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
	v, err := model.NewVerifier("ver-1", cred.ID(), enum.Password)
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
	v, err := model.NewVerifier("ver-1", cred.ID(), enum.Password)
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

// The minimum counts characters, the maximum counts bytes: bcrypt reads only
// the first 72.
func TestCheckPasswordPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		password string
		ok       bool
	}{
		{"11 bytes", strings.Repeat("a", 11), false},
		{"12 bytes", strings.Repeat("a", 12), true},
		{"72 bytes", strings.Repeat("a", 72), true},
		{"73 bytes", strings.Repeat("a", 73), false},
		{"11 two-byte characters", strings.Repeat("é", 11), false},
		{"12 two-byte characters", strings.Repeat("é", 12), true},
		{"24 three-byte characters, 72 bytes", strings.Repeat("€", 24), true},
		{"25 three-byte characters, 75 bytes", strings.Repeat("€", 25), false},
	} {
		err := CheckPasswordPolicy(tc.password)
		if tc.ok && err != nil {
			t.Errorf("%s: %v, want accepted", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, auth.ErrWeakPassword) {
			t.Errorf("%s: %v, want ErrWeakPassword", tc.name, err)
		}
	}
}
