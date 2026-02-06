package providers

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Provider authenticates a principal using a specific mechanism and returns an identity draft.
type Provider interface {
	Kind() kind.Auth
	Authenticate(ctx context.Context, req any) (*identity.Identity, error)
}
