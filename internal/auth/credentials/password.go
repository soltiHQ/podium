package credentials

import (
	"crypto/subtle"
	"errors"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"

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
	if cost <= 0 {
		cost = DefaultBcryptCost
	}

	cred, err := model.NewCredential(id, userID, kind.Password)
	if err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), cost)
	if err != nil {
		return nil, err
	}

	cred.SetSecret(PasswordHashKey, string(hash))
	return cred, nil
}

// VerifyPassword checks if the provided plaintext password matches the stored hash.
func VerifyPassword(cred *model.Credential, plainPassword string) error {
	if cred == nil {
		return ErrPasswordMismatch
	}
	if cred.AuthKind() != kind.Password {
		return ErrWrongAuthKind
	}

	hash, ok := cred.Secret(PasswordHashKey)
	if !ok || hash == "" {
		return ErrMissingPasswordHash
	}

	if subtle.ConstantTimeEq(int32(len(hash)), 0) == 1 {
		return ErrMissingPasswordHash
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return ErrPasswordMismatch
		}
		return err
	}
	return nil
}
