package session

import (
	"crypto/rand"
	"encoding/base64"

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
		return nil, ErrInvalidRefresh
	}
	h := sha3.New256()
	_, _ = h.Write([]byte(raw))
	return h.Sum(nil), nil
}

func constantTimeEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
