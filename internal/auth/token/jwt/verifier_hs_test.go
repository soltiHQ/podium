package jwt

import (
	"context"
	"errors"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/token"
)

func baseIdentity(now time.Time, iss, aud string) *identity.Identity {
	return &identity.Identity{
		IssuedAt:  now,
		NotBefore: now,
		ExpiresAt: now.Add(10 * time.Minute),

		Issuer:   iss,
		Audience: []string{aud},

		Subject:   "subj",
		UserID:    "user-1",
		TokenID:   "tid",
		SessionID: "sid",

		Permissions: []enum.Permission{"perm.read"},
	}
}

// Issue a token using real issuer implementation, so we don't guess claim mapping.
func mustIssue(t *testing.T, iss token.Issuer, id *identity.Identity) string {
	t.Helper()
	raw, err := iss.Issue(context.Background(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if raw == "" {
		t.Fatalf("Issue: empty token")
	}
	return raw
}

func TestHSVerifier_Verify_InvalidInputAndConfig(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: time.Unix(100, 0)}

	t.Run("empty token", func(t *testing.T) {
		v := NewHSVerifier("iss", "aud", []byte("s"), clk)
		_, err := v.Verify(ctx, "")
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("missing issuer", func(t *testing.T) {
		v := NewHSVerifier("", "aud", []byte("s"), clk)
		_, err := v.Verify(ctx, "x")
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("missing audience", func(t *testing.T) {
		v := NewHSVerifier("iss", "", []byte("s"), clk)
		_, err := v.Verify(ctx, "x")
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("empty secret", func(t *testing.T) {
		v := NewHSVerifier("iss", "aud", nil, clk)
		_, err := v.Verify(ctx, "x")
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})
}

func TestHSVerifier_Verify_Success_RoundTrip(t *testing.T) {
	ctx := context.Background()

	const (
		iss = "solti"
		aud = "control-plane"
	)
	clk := &fakeClock{t: time.Unix(200, 0)}
	secret := []byte("secret-1")

	issuer := NewHSIssuer(secret, clk)
	verifier := NewHSVerifier(iss, aud, secret, clk)

	id := baseIdentity(clk.Now(), iss, aud)
	raw := mustIssue(t, issuer, id)

	got, err := verifier.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got == nil {
		t.Fatalf("expected identity, got nil")
	}

	// Проверяем только то, что точно должно сохраняться через issuer->claims->verifier->identity
	if got.Issuer != iss {
		t.Fatalf("issuer mismatch: %q", got.Issuer)
	}
	if len(got.Audience) != 1 || got.Audience[0] != aud {
		t.Fatalf("audience mismatch: %#v", got.Audience)
	}
	if got.Subject != id.Subject {
		t.Fatalf("subject mismatch: %q != %q", got.Subject, id.Subject)
	}
	if got.UserID != id.UserID {
		t.Fatalf("user id mismatch: %q != %q", got.UserID, id.UserID)
	}
	if got.SessionID != id.SessionID {
		t.Fatalf("session id mismatch: %q != %q", got.SessionID, id.SessionID)
	}
	if got.TokenID == "" {
		t.Fatalf("expected token id")
	}
}

func TestHSVerifier_Verify_WrongSignature(t *testing.T) {
	ctx := context.Background()

	const (
		iss = "solti"
		aud = "control-plane"
	)
	clk := &fakeClock{t: time.Unix(300, 0)}
	secret := []byte("secret-1")
	wrongSecret := []byte("secret-2")

	issuer := NewHSIssuer(secret, clk)
	verifier := NewHSVerifier(iss, aud, wrongSecret, clk)

	id := baseIdentity(clk.Now(), iss, aud)
	raw := mustIssue(t, issuer, id)

	_, err := verifier.Verify(ctx, raw)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestHSVerifier_Verify_IssuerOrAudienceMismatch(t *testing.T) {
	ctx := context.Background()

	clk := &fakeClock{t: time.Unix(400, 0)}
	secret := []byte("secret-1")

	issuer := NewHSIssuer(secret, clk)

	id := baseIdentity(clk.Now(), "iss-A", "aud-A")
	raw := mustIssue(t, issuer, id)

	t.Run("issuer mismatch", func(t *testing.T) {
		v := NewHSVerifier("iss-B", "aud-A", secret, clk)
		_, err := v.Verify(ctx, raw)
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("audience mismatch", func(t *testing.T) {
		v := NewHSVerifier("iss-A", "aud-B", secret, clk)
		_, err := v.Verify(ctx, raw)
		if !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got %v", err)
		}
	})
}

func TestHSVerifier_Verify_ExpiredAndNotValidYet(t *testing.T) {
	ctx := context.Background()

	const (
		iss = "solti"
		aud = "control-plane"
	)
	clk := &fakeClock{t: time.Unix(500, 0)}
	secret := []byte("secret-1")

	issuer := NewHSIssuer(secret, clk)
	verifier := NewHSVerifier(iss, aud, secret, clk)

	t.Run("expired => ErrExpiredToken", func(t *testing.T) {
		id := baseIdentity(clk.Now(), iss, aud)
		// сделаем уже истекшим относительно clock.Now()
		id.ExpiresAt = clk.Now().Add(-1 * time.Minute)

		raw := mustIssue(t, issuer, id)
		_, err := verifier.Verify(ctx, raw)
		if !errors.Is(err, auth.ErrExpiredToken) {
			t.Fatalf("expected ErrExpiredToken, got %v", err)
		}
	})

	t.Run("not valid yet => ErrExpiredToken", func(t *testing.T) {
		id := baseIdentity(clk.Now(), iss, aud)
		id.NotBefore = clk.Now().Add(10 * time.Minute)

		raw := mustIssue(t, issuer, id)
		_, err := verifier.Verify(ctx, raw)
		if !errors.Is(err, auth.ErrExpiredToken) {
			t.Fatalf("expected ErrExpiredToken, got %v", err)
		}
	})
}

func TestHSVerifier_Verify_RejectsNonHS256(t *testing.T) {
	ctx := context.Background()

	const (
		iss = "solti"
		aud = "control-plane"
	)
	clk := &fakeClock{t: time.Unix(600, 0)}
	secret := []byte("secret-1")

	verifier := NewHSVerifier(iss, aud, secret, clk)

	claims := jwtlib.MapClaims{
		"iss": iss,
		"sub": "subj",
		"aud": []string{aud},
		"exp": clk.Now().Add(10 * time.Minute).Unix(),
		"nbf": clk.Now().Unix(),
		"iat": clk.Now().Unix(),
	}

	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodNone, claims)
	raw, err := tok.SignedString(jwtlib.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	_, err = verifier.Verify(ctx, raw)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestHSVerifier_SecretIsCopied(t *testing.T) {
	ctx := context.Background()

	const (
		iss = "solti"
		aud = "control-plane"
	)

	clk := &fakeClock{t: time.Unix(700, 0)}

	secret := []byte("orig-secret")
	issuer := NewHSIssuer(secret, clk)
	verifier := NewHSVerifier(iss, aud, secret, clk)

	for i := range secret {
		secret[i] = 'x'
	}

	id := baseIdentity(clk.Now(), iss, aud)
	raw := mustIssue(t, issuer, id)

	_, err := verifier.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("expected verify success, got %v", err)
	}
}
