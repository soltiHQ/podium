package providers

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
)

// Request is an auth-provider-specific request.
//
// Each provider defines its own concrete request type and must validate it using type assertions in Authenticate().
type Request interface {
	AuthKind() kind.Auth
}

// Result is the output of authentication (NOT authorization).
type Result struct {
	User       *model.User
	Credential *model.Credential
}

// Provider authenticates a principal using a specific mechanism (password, api_key, oidc, ...).
//
// Contract:
//   - Kind() must match the request's AuthKind() for supported requests.
//   - Authenticate must not perform authorization (RBAC) or token issuance.
//   - Authenticate must not mutate returned user/credential (treat as read-only).
type Provider interface {
	Kind() kind.Auth
	Authenticate(ctx context.Context, req Request) (*Result, error)
}
