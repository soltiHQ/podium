package providers

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
)

// Request is an auth-provider specific request.
type Request interface {
	AuthKind() kind.Auth
}

// Result is the output of authentication (NOT authorization).
type Result struct {
	User       *model.User
	Credential *model.Credential
}

// Provider authenticates a principal using a specific mechanism (password, api_key, oidc, ...).
type Provider interface {
	Kind() kind.Auth
	Authenticate(ctx context.Context, req Request) (*Result, error)
}
