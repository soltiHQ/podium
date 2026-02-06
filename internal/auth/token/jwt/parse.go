package jwt

import (
	"encoding/json"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
)

const (
	claimUserID      = "uid"
	claimSessionID   = "sid"
	claimPermissions = "perms"
)

func mapClaimsFromIdentity(id *identity.Identity, now time.Time) jwtlib.MapClaims {
	if id == nil {
		return jwtlib.MapClaims{}
	}

	if id.IssuedAt.IsZero() {
		id.IssuedAt = now
	}
	if id.NotBefore.IsZero() {
		id.NotBefore = id.IssuedAt
	}
	if id.ExpiresAt.IsZero() {
		id.ExpiresAt = id.IssuedAt
	}

	cl := token.Claims{
		Issuer:      id.Issuer,
		Audience:    id.Audience,
		Subject:     id.Subject,
		TokenID:     id.TokenID,
		SessionID:   id.SessionID,
		UserID:      id.UserID,
		IssuedAt:    id.IssuedAt,
		NotBefore:   id.NotBefore,
		ExpiresAt:   id.ExpiresAt,
		Permissions: id.Permissions,
	}
	return mapClaimsFromClaims(cl)
}

func identityFromMapClaims(mc jwtlib.MapClaims, issuer, audience string) (*identity.Identity, error) {
	sub, _ := mc["sub"].(string)
	uid, _ := mc[claimUserID].(string)
	jti, _ := mc["jti"].(string)
	sid, _ := mc[claimSessionID].(string)

	iat := time.Unix(int64FromClaim(mc["iat"]), 0)
	nbf := time.Unix(int64FromClaim(mc["nbf"]), 0)
	exp := time.Unix(int64FromClaim(mc["exp"]), 0)

	if sub == "" || uid == "" || jti == "" || sid == "" || exp.IsZero() {
		return nil, auth.ErrInvalidToken
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
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func mapClaimsFromClaims(cl token.Claims) jwtlib.MapClaims {
	mc := jwtlib.MapClaims{
		"iss": cl.Issuer,
		"sub": cl.Subject,
		"jti": cl.TokenID,

		"iat": cl.IssuedAt.Unix(),
		"nbf": cl.NotBefore.Unix(),
		"exp": cl.ExpiresAt.Unix(),

		claimUserID:    cl.UserID,
		claimSessionID: cl.SessionID,
	}

	if len(cl.Audience) == 1 {
		mc["aud"] = cl.Audience[0]
	} else if len(cl.Audience) > 1 {
		mc["aud"] = cl.Audience
	}

	if len(cl.Permissions) != 0 {
		perms := make([]string, 0, len(cl.Permissions))
		for _, p := range cl.Permissions {
			if p == "" {
				continue
			}
			perms = append(perms, string(p))
		}
		if len(perms) != 0 {
			mc[claimPermissions] = perms
		}
	}

	if mc["iss"] == "" {
		delete(mc, "iss")
	}
	if mc["jti"] == "" {
		delete(mc, "jti")
	}
	if mc[claimUserID] == "" {
		delete(mc, claimUserID)
	}
	if mc[claimSessionID] == "" {
		delete(mc, claimSessionID)
	}
	return mc
}
