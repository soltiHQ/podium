package dto

import (
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

type VerifierDTO struct {
	ID           string
	CredentialID string
	Auth         string // kind.Auth
	Data         map[string]string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func VerifierToDTO(v *model.Verifier) *VerifierDTO {
	data := make(map[string]string, len(v.DataAll()))
	for k, val := range v.DataAll() {
		data[k] = val
	}
	return &VerifierDTO{
		ID:           v.ID(),
		CredentialID: v.CredentialID(),
		Auth:         string(v.AuthKind()),
		Data:         data,
		CreatedAt:    v.CreatedAt(),
		UpdatedAt:    v.UpdatedAt(),
	}
}

func VerifierFromDTO(d *VerifierDTO) (*model.Verifier, error) {
	if d == nil {
		return nil, nil
	}
	v, err := model.NewVerifier(d.ID, d.CredentialID, kindAuth(d.Auth))
	if err != nil {
		return nil, err
	}
	for k, val := range d.Data {
		_ = v.DataSet(k, val)
	}
	v.SetCreatedAt(d.CreatedAt)
	v.SetUpdatedAt(d.UpdatedAt)
	return v, nil
}
