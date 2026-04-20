package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

type CredentialDTO struct {
	ID        string
	UserID    string
	Auth      string // kind.Auth
	Secrets   map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func CredentialToDTO(c *model.Credential) *CredentialDTO {
	secrets := make(map[string]string, len(c.SecretsAll()))
	for k, v := range c.SecretsAll() {
		secrets[k] = v
	}
	return &CredentialDTO{
		ID:        c.ID(),
		UserID:    c.UserID(),
		Auth:      string(c.AuthKind()),
		Secrets:   secrets,
		CreatedAt: c.CreatedAt(),
		UpdatedAt: c.UpdatedAt(),
	}
}

func CredentialFromDTO(d *CredentialDTO) (*model.Credential, error) {
	if d == nil {
		return nil, nil
	}
	c, err := model.NewCredential(d.ID, d.UserID, kindAuth(d.Auth))
	if err != nil {
		return nil, err
	}
	for k, v := range d.Secrets {
		_ = c.SetSecret(k, v)
	}
	c.SetCreatedAt(d.CreatedAt)
	c.SetUpdatedAt(d.UpdatedAt)
	return c, nil
}
