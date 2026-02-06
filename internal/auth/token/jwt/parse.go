package jwt

import (
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
)

const (
	claimUserID      = "uid"
	claimSessionID   = "sid"
	claimPermissions = "perms"
)

func mapClaimsFromIdentity(id *identity.Identity) jwtlib.MapClaims {
	var (
		iat = id.IssuedAt
		nbf = id.NotBefore
		exp = id.ExpiresAt
		now = time.Now()
	)
	if iat.IsZero() {
		iat = now
	}
	if nbf.IsZero() {
		nbf = iat
	}
	if exp.IsZero() {
		exp = iat.Add(15 * time.Minute)
	}

	perms := make([]string, 0, len(id.Permissions))
	for _, p := range id.Permissions {
		if p == "" {
			continue
		}
		perms = append(perms, string(p))
	}

	claims := jwtlib.MapClaims{
		"iat": iat.Unix(),
		"nbf": nbf.Unix(),
		"exp": exp.Unix(),

		"aud": id.Audience,
		"sub": id.Subject,
		"jti": id.TokenID,
		"iss": id.Issuer,

		claimUserID:      id.UserID,
		claimPermissions: perms,
	}
	if id.SessionID != "" {
		claims[claimSessionID] = id.SessionID
	}
	return claims
}

func identityFromMapClaims(mc jwtlib.MapClaims, issuer, audience string) (*identity.Identity, error) {
	var (
		sub, _ = mc["sub"].(string)
		uid, _ = mc[claimUserID].(string)
		jti, _ = mc["jti"].(string)
		sid, _ = mc[claimSessionID].(string)

		iat = time.Unix(int64FromClaim(mc["iat"]), 0)
		nbf = time.Unix(int64FromClaim(mc["nbf"]), 0)
		exp = time.Unix(int64FromClaim(mc["exp"]), 0)
	)
	if sub == "" || uid == "" || exp.IsZero() {
		return nil, token.ErrInvalidToken
	}
	
	id := &identity.Identity{
		IssuedAt:  iat,
		NotBefore: nbf,
		ExpiresAt: exp,

		Issuer:    issuer,
		Audience:  []string{audience},
		Subject:   sub,
		UserID:    uid,
		TokenID:   jti,
		SessionID: sid,
	}
	if raw, ok := mc[claimPermissions]; ok {
		id.Permissions = parsePermissions(raw)
	}
	return id, nil
}

func parsePermissions(v any) []kind.Permission {
	switch x := v.(type) {
	case []any:
		out := make([]kind.Permission, 0, len(x))
		for _, it := range x {
			s, ok := it.(string)
			if !ok || s == "" {
				continue
			}
			out = append(out, kind.Permission(s))
		}
		return out
	case []string:
		out := make([]kind.Permission, 0, len(x))
		for _, s := range x {
			if s == "" {
				continue
			}
			out = append(out, kind.Permission(s))
		}
		return out
	default:
		return nil
	}
}

func int64FromClaim(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case jsonNumber:
		return x.Int64()
	default:
		return 0
	}
}

type jsonNumber interface {
	Int64() int64
}
