package jwt

type Claims struct {
	Issuer    string   `json:"iss,omitempty"`
	Audience  []string `json:"aud,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Expiry    int64    `json:"exp,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	TokenID   string   `json:"jti,omitempty"`

	UserID      string   `json:"uid,omitempty"`
	Permissions []string `json:"perms,omitempty"`
}
