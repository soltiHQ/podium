package credentials

import (
	"errors"

	"golang.org/x/crypto/bcrypt"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
)

const (
	PasswordHashKey   = "hash"
	DefaultBcryptCost = 12
)

func normalizeCost(cost int) (int, error) {
	if cost <= 0 {
		return DefaultBcryptCost, nil
	}
	if cost < bcrypt.MinCost {
		return bcrypt.MinCost, nil
	}
	if cost > bcrypt.MaxCost {
		return 0, auth.ErrInvalidRequest
	}
	return cost, nil
}

// NewPasswordCredential creates a credential bound to user with password kind.
// No secret material is stored here.
func NewPasswordCredential(id, userID string) (*model.Credential, error) {
	return model.NewCredential(id, userID, kind.Password)
}

// NewPasswordVerifier creates a verifier containing bcrypt hash.
func NewPasswordVerifier(
	verifierID string,
	credentialID string,
	plainPassword string,
	cost int,
) (*model.Verifier, error) {
	if plainPassword == "" {
		return nil, auth.ErrInvalidRequest
	}

	cost, err := normalizeCost(cost)
	if err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), cost)
	if err != nil {
		return nil, err
	}

	v, err := model.NewVerifier(verifierID, credentialID, kind.Password)
	if err != nil {
		return nil, err
	}

	if err = v.DataSet(PasswordHashKey, string(hash)); err != nil {
		return nil, err
	}
	return v, nil
}

// VerifyPassword verifies plaintext password against stored verifier.
func VerifyPassword(
	cred *model.Credential,
	v *model.Verifier,
	plainPassword string,
) error {
	if cred == nil || v == nil || plainPassword == "" {
		return auth.ErrPasswordMismatch
	}

	if cred.AuthKind() != kind.Password ||
		v.AuthKind() != kind.Password {
		return auth.ErrWrongAuthKind
	}

	if v.CredentialID() != cred.ID() {
		return auth.ErrPasswordMismatch
	}

	hash, ok := v.DataGet(PasswordHashKey)
	if !ok || hash == "" {
		return auth.ErrMissingPasswordHash
	}

	err := bcrypt.CompareHashAndPassword(
		[]byte(hash),
		[]byte(plainPassword),
	)

	if err == nil {
		return nil
	}

	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return auth.ErrPasswordMismatch
	}

	return auth.ErrMissingPasswordHash
}
