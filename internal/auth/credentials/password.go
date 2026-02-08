package credentials

import (
	"errors"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"

	"golang.org/x/crypto/bcrypt"
)

const (
	// PasswordHashKey stores bcrypt hash inside a credential record.
	PasswordHashKey = "hash"
	// DefaultBcryptCost is a reasonable default cost.
	DefaultBcryptCost = 12
)

// NewPasswordCredential creates a password credential with bcrypt-hashed password.
func NewPasswordCredential(id, userID, plainPassword string, cost int) (*model.Credential, error) {
	if plainPassword == "" {
		return nil, auth.ErrInvalidRequest
	}
	if cost <= 0 {
		cost = DefaultBcryptCost
	}
	if cost < bcrypt.MinCost {
		cost = bcrypt.MinCost
	}
	if cost > bcrypt.MaxCost {
		return nil, auth.ErrInvalidRequest
	}

	cred, err := model.NewCredential(id, userID, kind.Password)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), cost)
	if err != nil {
		return nil, err
	}
	if err = cred.SetSecret(PasswordHashKey, string(hash)); err != nil {
		return nil, err
	}
	return cred, nil
}

// VerifyPassword checks if the provided plaintext password matches the stored hash.
func VerifyPassword(cred *model.Credential, plainPassword string) error {
	if cred == nil {
		return auth.ErrPasswordMismatch
	}
	if cred.AuthKind() != kind.Password {
		return auth.ErrWrongAuthKind
	}

	hash, ok := cred.Secret(PasswordHashKey)
	if !ok || hash == "" {
		return auth.ErrMissingPasswordHash
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword))
	if err == nil {
		return nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return auth.ErrPasswordMismatch
	}
	return auth.ErrMissingPasswordHash
}
