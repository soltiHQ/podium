package session

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/soltiHQ/control-plane/internal/auth"
)

func TestHashRefreshToken_Empty(t *testing.T) {
	t.Parallel()

	_, err := hashRefreshToken("")
	if !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("expected ErrInvalidRefresh, err=%v", err)
	}
}

func TestNewRefreshToken_GeneratesValidTokenAndHash(t *testing.T) {
	t.Parallel()

	raw, h, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken err=%v", err)
	}
	if raw == "" {
		t.Fatalf("expected non-empty raw token")
	}
	if len(h) == 0 {
		t.Fatalf("expected non-empty hash")
	}
	if len(h) != 32 { // sha3-256
		t.Fatalf("expected 32-byte hash, got=%d", len(h))
	}

	// token must be valid base64url (raw, no padding) and decode to 32 bytes
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("raw token is not valid base64url: %v", err)
	}
	if len(b) != 32 {
		t.Fatalf("expected 32 decoded bytes, got=%d", len(b))
	}

	// hash must match hashRefreshToken(raw)
	h2, err := hashRefreshToken(raw)
	if err != nil {
		t.Fatalf("hashRefreshToken err=%v", err)
	}
	if !bytes.Equal(h, h2) {
		t.Fatalf("hash mismatch: newRefreshToken hash != hashRefreshToken(raw)")
	}
}

func TestNewRefreshToken_TokensAreDifferent(t *testing.T) {
	t.Parallel()

	raw1, h1, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken(1) err=%v", err)
	}
	raw2, h2, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken(2) err=%v", err)
	}

	if raw1 == raw2 {
		t.Fatalf("expected different raw tokens")
	}
	if bytes.Equal(h1, h2) {
		t.Fatalf("expected different hashes")
	}
}

func TestHashRefreshToken_Deterministic(t *testing.T) {
	t.Parallel()

	// any non-empty string works; use a stable one
	raw := "0123456789abcdef"

	h1, err := hashRefreshToken(raw)
	if err != nil {
		t.Fatalf("hashRefreshToken(1) err=%v", err)
	}
	h2, err := hashRefreshToken(raw)
	if err != nil {
		t.Fatalf("hashRefreshToken(2) err=%v", err)
	}

	if !bytes.Equal(h1, h2) {
		t.Fatalf("expected deterministic hash")
	}
	if len(h1) != 32 {
		t.Fatalf("expected 32-byte hash, got=%d", len(h1))
	}
}
