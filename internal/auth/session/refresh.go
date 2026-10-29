package session

import (
	"crypto/rand"
	"crypto/sha3"
	"encoding/base64"

	"github.com/soltiHQ/control-plane/internal/auth"
)

const (
	refreshTokenBytes = 32
)

// newRefreshToken generates a new refresh token and its hash suitable for storage.
func newRefreshToken() (raw string, hash []byte, err error) {
	var b [refreshTokenBytes]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])

	h := sha3.New256()
	_, _ = h.Write([]byte(raw))
	hash = h.Sum(nil)
	return raw, hash, nil
}

// hashRefreshToken hashes a provided raw refresh token value.
func hashRefreshToken(raw string) ([]byte, error) {
	if raw == "" {
		return nil, auth.ErrInvalidRefresh
	}
	h := sha3.New256()
	_, _ = h.Write([]byte(raw))
	return h.Sum(nil), nil
}
