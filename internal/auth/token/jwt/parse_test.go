package jwt

import (
	"errors"
	"testing"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/soltiHQ/control-plane/internal/auth"
)

// Behind the parser's own check, claims without exp still yield no identity.
func TestIdentityFromMapClaimsRequiresExp(t *testing.T) {
	mc := jwtlib.MapClaims{"sub": "subj", "uid": "user-1", "jti": "tid", "sid": "sid"}
	if _, err := identityFromMapClaims(mc, "solti", "control-plane"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("claims without exp: %v, want ErrInvalidToken", err)
	}
	mc["exp"] = float64(900)
	if _, err := identityFromMapClaims(mc, "solti", "control-plane"); err != nil {
		t.Fatalf("claims with exp: %v", err)
	}
}
