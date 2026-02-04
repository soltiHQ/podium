package authenticator

import (
	"context"

	"github.com/soltiHQ/control-plane/auth"
)

// Authenticator validates user-provided credentials and issues an access token.
//
// Implementations may support multiple authentication mechanisms (password, API key, OIDC, etc.).
// On success, it returns a signed token and the corresponding authenticated identity.
//
// Expected behavior:
//   - Return an error for invalid credentials (do not leak which field failed).
//   - Return a non-nil Identity when token issuance succeeds.
//   - The returned token should embed permissions and other authorization claims
//     required for request processing without additional storage lookups.
type Authenticator interface {
	// Authenticate validates the request and returns a signed access token and identity.
	Authenticate(ctx context.Context, req *Request) (token string, id *auth.Identity, err error)
}

// Request contains input credentials for authentication.
type Request struct {
	// Subject is a stable principal identifier (typically mapped to user.Subject()).
	Subject string
	// Password is a plaintext password provided by the caller.
	Password string
}
