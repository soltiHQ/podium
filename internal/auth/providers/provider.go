package providers

import (
	"context"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

// Request is an auth-provider-specific request.
//
// Each provider defines its own concrete request type and must validate it using type assertions in Authenticate().
//
// AuthKind must return the authentication mechanism that this request targets.
// It is used by the provider to ensure that:
//
//   - The request is intended for the specific provider implementation.
//   - The provider's Kind() matches the request's AuthKind().
//   - Mismatched request/provider combinations are rejected with ErrInvalidRequest.
type Request interface {
	// AuthKind returns the authentication mechanism this request is intended for (e.g., enum.Password, enum.APIKey).
	AuthKind() enum.Auth
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
	Kind() enum.Auth
	Authenticate(ctx context.Context, req Request) (*Result, error)
}
