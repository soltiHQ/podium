package auth

import (
	"errors"
	"net/http"
	"strings"

	authcore "github.com/soltiHQ/control-plane/auth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// extractBearerToken parses Authorization header and returns the Bearer token.
// Returns an empty string if the header is missing or malformed.
func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}

	parts := strings.Fields(header)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// httpErrorFromAuth maps auth errors to HTTP status code and a short message
// that can be safely returned to the client.
func httpErrorFromAuth(err error) (int, string) {
	switch {
	case errors.Is(err, authcore.ErrInvalidToken),
		errors.Is(err, authcore.ErrExpiredToken):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, authcore.ErrUnauthorized):
		return http.StatusForbidden, "forbidden"
	default:
		return http.StatusUnauthorized, "unauthorized"
	}
}

// grpcErrorFromAuth maps auth errors to gRPC status errors.
func grpcErrorFromAuth(err error) error {
	switch {
	case errors.Is(err, authcore.ErrInvalidToken),
		errors.Is(err, authcore.ErrExpiredToken):
		return status.Error(codes.Unauthenticated, "unauthenticated")
	case errors.Is(err, authcore.ErrUnauthorized):
		return status.Error(codes.PermissionDenied, "permission denied")
	default:
		return status.Error(codes.Unauthenticated, "unauthenticated")
	}
}
