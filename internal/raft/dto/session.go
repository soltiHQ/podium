package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

type SessionDTO struct {
	ID           string
	UserID       string
	CredentialID string
	Auth         string // kind.Auth
	RefreshHash  []byte
	ExpiresAt    time.Time
	RevokedAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func SessionToDTO(s *model.Session) *SessionDTO {
	hash := s.RefreshHash()
	cp := make([]byte, len(hash))
	copy(cp, hash)
	return &SessionDTO{
		ID:           s.ID(),
		UserID:       s.UserID(),
		CredentialID: s.CredentialID(),
		Auth:         string(s.AuthKind()),
		RefreshHash:  cp,
		ExpiresAt:    s.ExpiresAt(),
		RevokedAt:    s.RevokedAt(),
		CreatedAt:    s.CreatedAt(),
		UpdatedAt:    s.UpdatedAt(),
	}
}

func SessionFromDTO(d *SessionDTO) (*model.Session, error) {
	if d == nil {
		return nil, nil
	}
	hash := make([]byte, len(d.RefreshHash))
	copy(hash, d.RefreshHash)
	s, err := model.NewSession(d.ID, d.UserID, d.CredentialID, kindAuth(d.Auth), hash, d.ExpiresAt)
	if err != nil {
		return nil, err
	}
	s.SetCreatedAt(d.CreatedAt)
	s.SetUpdatedAt(d.UpdatedAt)
	s.SetRevokedAt(d.RevokedAt)
	return s, nil
}
