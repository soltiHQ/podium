package token

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/auth/identity"
)

// Issuer encodes and signs an identity into a raw access token string.
type Issuer interface {
	Issue(ctx context.Context, id *identity.Identity) (string, error)
}
