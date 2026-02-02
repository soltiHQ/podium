package jwt

import "github.com/soltiHQ/control-plane/auth"

type Verifier struct {
	cfg auth.JWTConfig
}

func NewVerifier(cfg auth.JWTConfig) *Verifier {
	return &Verifier{cfg: cfg}
}
