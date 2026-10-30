package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Enrollment refuses a token over 1024 bytes and stores nothing. The cap is an
// enrollment rule only: a credential stored with a longer token, by an older
// version, still verifies.
func TestVerifyOrEnrollTokenCapsOnlyNewTokens(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	svc := New(store, zerolog.New(io.Discard))

	if err := svc.VerifyOrEnrollToken(ctx, "agent-a", strings.Repeat("t", 1025)); !errors.Is(err, ErrTokenTooLong) {
		t.Fatalf("enroll a 1025-byte token: %v, want ErrTokenTooLong", err)
	}
	if token, err := svc.AgentToken(ctx, "agent-a"); err != nil || token != "" {
		t.Fatalf("refused enrollment stored a token: %d bytes, %v", len(token), err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-a", strings.Repeat("t", 1024)); err != nil {
		t.Fatalf("enroll a 1024-byte token: %v", err)
	}

	stored := strings.Repeat("s", 4096)
	legacy, err := model.NewAgentCredential("agent-b", stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgentCredential(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-b", stored); err != nil {
		t.Fatalf("verify a stored 4096-byte token: %v", err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-b", "other-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong token for agent-b: %v, want ErrUnauthenticated", err)
	}
}

// Hand-derived fixtures: HMAC-SHA256 of the agent ID under the secret as
// lowercase hex, computed outside podium (openssl dgst -sha256 -hmac), so the
// tests cannot agree with a wrong derivation.
const (
	enrollSecretA = "0123456789abcdef0123456789abcdef"
	enrollSecretB = "a-different-secret-of-32-bytes-or-more!"

	tokenA1 = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // secret A, "agent-1"
	tokenA2 = "14202fea7ea2cc313572d5d8e8c424489c954421b0f6160ce9e07d27340644fc" // secret A, "agent-2"
	tokenB1 = "2cf853a3b3749aa4f2caf958c77a8eeec3ac9402ad91861afe43f9cbaaa1e12d" // secret B, "agent-1"
	tokenB2 = "d6ddd1b414a70ccd72743b37c45744a79a0ec8def0c24f86a5f773f955c309fe" // secret B, "agent-2"
)

// The enrollment token is the lowercase hex of HMAC-SHA256 keyed by the secret
// with the agent ID as the message: a swapped key and message, another hash or
// another encoding gives a different value.
func TestEnrollTokenIsHexHMACOfTheAgentID(t *testing.T) {
	for _, tc := range []struct{ secret, id, want string }{
		{enrollSecretA, "agent-1", tokenA1},
		{enrollSecretA, "agent-2", tokenA2},
		{enrollSecretA, "Agent-1", "a61f91530932f424d7699cecca687710b773ca4ff349246a2082d08319c41d6b"},
		{enrollSecretB, "agent-1", tokenB1},
	} {
		if got := EnrollToken(tc.secret, tc.id); got != tc.want {
			t.Errorf("EnrollToken(%q, %q) = %q, want %q", tc.secret, tc.id, got, tc.want)
		}
	}
}

// With an enrollment secret an unknown agent ID enrolls only with the token
// derived from it for that ID. Anything else is unauthenticated and stores
// nothing; the cap on the length keeps its own error.
func TestVerifyOrEnrollTokenWithSecret(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, token string
		want        error
	}{
		{"derived token", tokenA1, nil},
		{"no token", "", ErrUnauthenticated},
		{"made-up token", "made-up-token", ErrUnauthenticated},
		{"token derived for another agent", tokenA2, ErrUnauthenticated},
		{"token derived under another secret", tokenB1, ErrUnauthenticated},
		{"uppercase hex", strings.ToUpper(tokenA1), ErrUnauthenticated},
		{"trailing newline", tokenA1 + "\n", ErrUnauthenticated},
		{"prefix of the token", tokenA1[:63], ErrUnauthenticated},
		{"token over the cap", strings.Repeat("t", 1025), ErrTokenTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(inmemory.New(), zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyOrEnrollToken: %v, want %v", err, tc.want)
			}
			stored, err := svc.AgentToken(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			want := "" // only the derived token is stored
			if tc.want == nil {
				want = tc.token
			}
			if stored != want {
				t.Fatalf("stored token %q, want %q", stored, want)
			}
		})
	}
}

// The secret gates enrollment and nothing else. An agent that has a credential
// is checked against it as without a secret: a token stored before the secret
// was set, or under an earlier secret, keeps working, and the token derived
// today is no stand-in for it. Dropping the secret changes nothing either.
func TestKnownAgentsUnaffectedBySecret(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)

	if err := New(store, logger).VerifyOrEnrollToken(ctx, "agent-1", "legacy-token"); err != nil {
		t.Fatalf("enroll agent-1 without a secret: %v", err)
	}
	if err := New(store, logger, WithEnrollSecret(enrollSecretA)).VerifyOrEnrollToken(ctx, "agent-2", tokenA2); err != nil {
		t.Fatalf("enroll agent-2 under secret A: %v", err)
	}

	rotated := New(store, logger, WithEnrollSecret(enrollSecretB))
	for _, tc := range []struct {
		name, id, token string
		svc             *Service
		want            error
	}{
		{"stored token of an agent enrolled before the secret", "agent-1", "legacy-token", rotated, nil},
		{"derived token as stand-in for it", "agent-1", tokenB1, rotated, ErrUnauthenticated},
		{"stored token of an agent enrolled under an earlier secret", "agent-2", tokenA2, rotated, nil},
		{"derived token of the current secret as stand-in", "agent-2", tokenB2, rotated, ErrUnauthenticated},
		{"stored token after the secret is dropped", "agent-2", tokenA2, New(store, logger), nil},
		{"no token", "agent-1", "", rotated, ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.svc.VerifyOrEnrollToken(ctx, tc.id, tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyOrEnrollToken: %v, want %v", err, tc.want)
			}
		})
	}
	// Checking never rewrites a stored token.
	for id, want := range map[string]string{"agent-1": "legacy-token", "agent-2": tokenA2} {
		if got, err := rotated.AgentToken(ctx, id); err != nil || got != want {
			t.Errorf("stored token of %s: %q %v, want %q", id, got, err, want)
		}
	}
}
