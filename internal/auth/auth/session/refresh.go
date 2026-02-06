package session

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/soltiHQ/control-plane/internal/auth"
	"golang.org/x/crypto/sha3"
)

func newRefreshToken() (raw string, hash []byte, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])

	h := sha3.New256()
	_, _ = h.Write([]byte(raw))
	hash = h.Sum(nil)
	return raw, hash, nil
}

func hashRefreshToken(raw string) ([]byte, error) {
	if raw == "" {
		return nil, auth.ErrInvalidRefresh
	}
	h := sha3.New256()
	_, _ = h.Write([]byte(raw))
	return h.Sum(nil), nil
}
