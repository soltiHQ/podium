package apimap

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func Session(s *model.Session) restv1.Session {
	if s == nil {
		return restv1.Session{}
	}
	return restv1.Session{
		CreatedAt: s.CreatedAt(),
		UpdatedAt: s.UpdatedAt(),
		ExpiresAt: s.ExpiresAt(),
		RevokedAt: s.RevokedAt(),

		ID:           s.ID(),
		UserID:       s.UserID(),
		CredentialID: s.CredentialID(),
		AuthKind:     string(s.AuthKind()),

		Revoked: s.Revoked(),
	}
}
