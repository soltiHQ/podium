package model

import (
	"time"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/kind"
)

var _ domain.Entity[*Credential] = (*Credential)(nil)

// Credential is a core domain entity that represents a single authentication method
// bound to a specific user (e.g., password, api_key, oidc, etc.).
type Credential struct {
	createdAt time.Time
	updatedAt time.Time

	id     string
	userID string

	secrets map[string]string
	auth    kind.Auth
}

// NewCredential creates a new credential domain model.
func NewCredential(id, userID string, auth kind.Auth) (*Credential, error) {
	if id == "" {
		return nil, domain.ErrEmptyID
	}
	if userID == "" {
		return nil, domain.ErrEmptyUserID
	}

	now := time.Now()
	return &Credential{
		createdAt: now,
		updatedAt: now,
		id:        id,
		userID:    userID,
		auth:      auth,
	}, nil
}

// ID returns the unique identifier of the credential entity.
func (c *Credential) ID() string { return c.id }

// UserID returns the identifier of the user this credential belongs to.
func (c *Credential) UserID() string { return c.userID }

// AuthKind returns the authentication kind associated with this credential (e.g., password, api_key, oidc).
func (c *Credential) AuthKind() kind.Auth { return c.auth }

// CreatedAt returns the timestamp when the credential was created.
func (c *Credential) CreatedAt() time.Time { return c.createdAt }

// UpdatedAt returns the timestamp of the last modification to the credential.
func (c *Credential) UpdatedAt() time.Time { return c.updatedAt }

// Secret returns a secret value by key.
func (c *Credential) Secret(key string) (string, bool) {
	v, ok := c.secrets[key]
	return v, ok
}

// SecretsAll returns a shallow copy of secrets map.
// Use carefully: values are still strings, safe to copy.
func (c *Credential) SecretsAll() map[string]string {
	out := make(map[string]string, len(c.secrets))
	for k, v := range c.secrets {
		out[k] = v
	}
	return out
}

// SetSecret sets a secret value by key and bumps UpdatedAt.
func (c *Credential) SetSecret(key, value string) error {
	if key == "" {
		return domain.ErrFieldEmpty
	}
	if value == "" {
		return domain.ErrFieldEmpty
	}
	c.secrets[key] = value
	c.updatedAt = time.Now()
	return nil
}

// DeleteSecret removes a secret value by key and bumps UpdatedAt if it existed.
//
// The operation is idempotent: deleting a missing key is a no-op.
func (c *Credential) DeleteSecret(key string) error {
	if key == "" {
		return domain.ErrFieldEmpty
	}
	if c.secrets == nil {
		return nil
	}
	if _, ok := c.secrets[key]; !ok {
		return nil
	}
	delete(c.secrets, key)
	c.updatedAt = time.Now()
	return nil
}

// Clone creates a deep copy of the credential model.
func (c *Credential) Clone() *Credential {
	return &Credential{
		createdAt: c.createdAt,
		updatedAt: c.updatedAt,
		id:        c.id,
		userID:    c.userID,
		auth:      c.auth,
	}
}
