package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
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
	legacy, err := model.NewStoredAgentCredential("agent-b", stored)
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

// Hand-derived fixtures, computed outside podium (openssl dgst -sha256 -hmac),
// so the tests cannot agree with a wrong derivation. A token is the lowercase
// hex of HMAC-SHA256 under the secret of the agent ID at generation 0, and of
// the agent ID, a NUL byte and the generation in decimal at a generation of 1
// or more. A secret ID is the first 8 bytes of HMAC-SHA256 of
// "podium-agent-secret-id-v1" under the secret, in hex.
const (
	enrollSecretA = "0123456789abcdef0123456789abcdef"
	enrollSecretB = "a-different-secret-of-32-bytes-or-more!"

	tokenA1 = "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058" // secret A, "agent-1"
	tokenA2 = "14202fea7ea2cc313572d5d8e8c424489c954421b0f6160ce9e07d27340644fc" // secret A, "agent-2"
	tokenB1 = "2cf853a3b3749aa4f2caf958c77a8eeec3ac9402ad91861afe43f9cbaaa1e12d" // secret B, "agent-1"

	tokenA1g1 = "7f6129ad9f0126964aafb88a75a2019467059c1ae9d20b931b465d3aee996fae" // secret A, "agent-1", generation 1
	tokenB1g1 = "b64fbdc7c60d9020dba30027edf39b4aaf558e9fe5c69dc5a691ef40420c4c11" // secret B, "agent-1", generation 1

	secretIDA = "26d90767da091ed4"
	secretIDB = "e2245cbd7c9b04d5"
)

// Generation 0 derives the token that podium agent-token printed before there
// were generations: HMAC-SHA256 keyed by the secret with the agent ID as the
// message, as lowercase hex, so every token handed out before stays valid. A
// swapped key and message, another hash or another encoding gives a different
// value. A generation of 1 or more adds a NUL byte and the generation in decimal
// to the message, so each generation has a token of its own.
func TestDeriveTokenGenerationZeroIsEnrollToken(t *testing.T) {
	for _, tc := range []struct {
		secret, id string
		generation uint32
		want       string
	}{
		{enrollSecretA, "agent-1", 0, tokenA1},
		{enrollSecretA, "agent-2", 0, tokenA2},
		{enrollSecretA, "Agent-1", 0, "a61f91530932f424d7699cecca687710b773ca4ff349246a2082d08319c41d6b"},
		{enrollSecretB, "agent-1", 0, tokenB1},
		{enrollSecretA, "agent-1", 1, tokenA1g1},
		{enrollSecretA, "agent-1", 2, "089a433310152c9aa06043ed2151343c55f47fa0da583cfcbd14d149070e51e0"},
		{enrollSecretA, "agent-1", 10, "6e927593631b57b7e8d62c03b871f4232013b1f4bbf462d74b427f7ff8d23f60"},
		{enrollSecretA, "agent-1", math.MaxUint32, "d7c3f7836e2a72c6dcd036fc0649a2ec86367eb8991fa1b3185e55e7eb0106e0"},
		{enrollSecretA, "agent-2", 1, "e52ece96b54989fac2ee70fe7b5aa19e635a408c4fd87532cfe399e66d8c7a26"},
		{enrollSecretB, "agent-1", 1, tokenB1g1},
	} {
		if got := DeriveToken(tc.secret, tc.id, tc.generation); got != tc.want {
			t.Errorf("DeriveToken(%q, %q, %d) = %q, want %q", tc.secret, tc.id, tc.generation, got, tc.want)
		}
	}
}

// The secret ID tells two secrets apart: the first 8 bytes of HMAC-SHA256 of a
// fixed label under the secret.
func TestSecretIDNamesTheSecret(t *testing.T) {
	for secret, want := range map[string]string{enrollSecretA: secretIDA, enrollSecretB: secretIDB} {
		if got := hex.EncodeToString(SecretID(secret)); got != want {
			t.Errorf("SecretID(%q) = %s, want %s", secret, got, want)
		}
	}
}

// otherSecret and noSecret are the texts of the DerivedCredentialError of
// agent-1 at generation 0: under another secret, and with no secret set.
const (
	otherSecret = "agent agent-1 enrolled under another enrollment secret: it needs the token that podium agent-token --generation 0 agent-1 prints with the current secret"
	noSecret    = "agent agent-1 holds a credential derived from an enrollment secret, and no enrollment secret is set: it needs the secret it enrolled under"
)

// With an enrollment secret an unknown agent ID enrolls only with the token
// derived from it for that ID, at generation 0, and the credential it gets is a
// derived one: the generation and the ID of the secret, never the token.
// Anything else is unauthenticated and stores nothing; the cap on the length
// keeps its own error.
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
		{"token of generation 1", tokenA1g1, ErrUnauthenticated},
		{"uppercase hex", strings.ToUpper(tokenA1), ErrUnauthenticated},
		{"trailing newline", tokenA1 + "\n", ErrUnauthenticated},
		{"prefix of the token", tokenA1[:63], ErrUnauthenticated},
		{"token over the cap", strings.Repeat("t", 1025), ErrTokenTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			svc := New(store, zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tc.token); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyOrEnrollToken: %v, want %v", err, tc.want)
			}
			cred, err := store.GetAgentCredential(ctx, "agent-1")
			if tc.want != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("a refused enrollment stored a credential: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !cred.Derived() || cred.Generation() != 0 || hex.EncodeToString(cred.SecretID()) != secretIDA || cred.Token() != "" {
				t.Fatalf("enrolled credential: derived %v, generation %d, secret ID %x, %d-byte token; want derived at 0 under %s, no token",
					cred.Derived(), cred.Generation(), cred.SecretID(), len(cred.Token()), secretIDA)
			}
			if token, err := svc.AgentToken(ctx, "agent-1"); err != nil || token != tc.token {
				t.Fatalf("AgentToken: %q %v, want the derived token", token, err)
			}
		})
	}
}

// Without an enrollment secret an unknown agent ID enrolls the token it
// presents (trust on first use), and the credential stores it.
func TestVerifyOrEnrollTokenWithoutSecretStoresTheToken(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	svc := New(store, zerolog.New(io.Discard))
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", "chosen-token"); err != nil {
		t.Fatal(err)
	}
	cred, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil || cred.Derived() || cred.Token() != "chosen-token" {
		t.Fatalf("enrolled credential: %v, want a stored one with the token", err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", "another-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("another token: %v, want ErrUnauthenticated", err)
	}
}

// A stored credential is compared as before, whether a secret is set or not:
// a token stored before the secret was set keeps working, and the token
// derived today is no stand-in for it. Checking it rewrites nothing.
func TestStoredCredentialIsComparedAsStored(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	if err := New(store, logger).VerifyOrEnrollToken(ctx, "agent-1", "legacy-token"); err != nil {
		t.Fatalf("enroll agent-1 without a secret: %v", err)
	}
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	for _, tc := range []struct {
		name, token string
		svc         *Service
		want        error
	}{
		{"its token, no secret", "legacy-token", New(store, logger), nil},
		{"its token, a secret set", "legacy-token", withA, nil},
		{"the derived token as stand-in", tokenA1, withA, ErrUnauthenticated},
		{"no token", "", withA, ErrUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.VerifyOrEnrollToken(ctx, "agent-1", tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifyOrEnrollToken: %v, want %v", err, tc.want)
			}
			var credErr *DerivedCredentialError
			if errors.As(err, &credErr) {
				t.Fatalf("a stored credential refused with %v, want a plain ErrUnauthenticated", err)
			}
		})
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || cred.Derived() || cred.Token() != "legacy-token" {
		t.Fatalf("credential of agent-1 after the checks: %v, want it stored with its token", err)
	}
}

// A derived credential needs the secret it derives from. Under another secret,
// or with none, its agent is refused, as an ErrUnauthenticated, with a
// DerivedCredentialError that names the agent and says what it needs, and
// AgentToken fails with the same error, so podium makes no call to it. A wrong
// token under its own secret is a plain ErrUnauthenticated.
func TestDerivedCredentialNeedsItsSecret(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatalf("enroll agent-1 under secret A: %v", err)
	}
	withB := New(store, logger, WithEnrollSecret(enrollSecretB))
	without := New(store, logger)
	for _, tc := range []struct {
		name, token string
		svc         *Service
		want        string // the text of the DerivedCredentialError, "" for none
		refused     bool
	}{
		{"its token under its secret", tokenA1, withA, "", false},
		{"a made-up token under its secret", "made-up-token", withA, "", true},
		{"the token of generation 1 under its secret", tokenA1g1, withA, "", true},
		{"its token under another secret", tokenA1, withB, otherSecret, true},
		{"its token with no secret", tokenA1, without, noSecret, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.VerifyOrEnrollToken(ctx, "agent-1", tc.token)
			if !tc.refused {
				if err != nil {
					t.Fatalf("VerifyOrEnrollToken: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("VerifyOrEnrollToken: %v, want ErrUnauthenticated", err)
			}
			var credErr *DerivedCredentialError
			switch {
			case tc.want == "" && errors.As(err, &credErr):
				t.Fatalf("refused with %q, want a plain ErrUnauthenticated", err)
			case tc.want != "" && (!errors.As(err, &credErr) || err.Error() != tc.want):
				t.Fatalf("refused with %q, want the DerivedCredentialError %q", err, tc.want)
			}
		})
	}

	if token, err := withA.AgentToken(ctx, "agent-1"); err != nil || token != tokenA1 {
		t.Fatalf("AgentToken under secret A: %q %v, want the derived token", token, err)
	}
	for _, tc := range []struct {
		name string
		svc  *Service
		want string
	}{{"another secret", withB, otherSecret}, {"no secret", without, noSecret}} {
		token, err := tc.svc.AgentToken(ctx, "agent-1")
		var credErr *DerivedCredentialError
		if token != "" || !errors.As(err, &credErr) || err.Error() != tc.want || !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("AgentToken under %s: %q %v, want no token and the error %q", tc.name, token, err, tc.want)
		}
		if kind := errkind.Classify(err); kind != errkind.FailedPrecondition {
			t.Fatalf("AgentToken under %s: the error classifies as %v, want FailedPrecondition, not a 401 of the API caller", tc.name, kind)
		}
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || hex.EncodeToString(cred.SecretID()) != secretIDA {
		t.Fatalf("credential of agent-1 after the refusals: %v, want it still under secret A", err)
	}
}

// An agent re-provisioned under the current secret, with the token that podium
// agent-token prints for its generation, syncs again: its credential moves to
// the current secret, at the same generation, in the transaction that checks
// it, and podium presents that token from then on. The token of the old secret
// no longer works.
func TestReprovisionedAgentMovesToTheCurrentSecret(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatal(err)
	}
	enrolled, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}

	counted := &countingTx{Storage: store}
	withB := New(counted, logger, WithEnrollSecret(enrollSecretB))
	if err := withB.VerifyOrEnrollToken(ctx, "agent-1", tokenB1); err != nil {
		t.Fatalf("agent-1 with the token of secret B: %v", err)
	}
	if counted.txs != 1 {
		t.Fatalf("the check took %d transactions, want 1", counted.txs)
	}
	cred, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil || !cred.Derived() || cred.Generation() != 0 || hex.EncodeToString(cred.SecretID()) != secretIDB || cred.Token() != "" {
		t.Fatalf("credential after the re-provisioning: %v, want derived at 0 under %s", err, secretIDB)
	}
	if !cred.CreatedAt().Equal(enrolled.CreatedAt()) {
		t.Fatalf("the credential's enrollment time moved: %v, want %v", cred.CreatedAt(), enrolled.CreatedAt())
	}
	if token, err := withB.AgentToken(ctx, "agent-1"); err != nil || token != tokenB1 {
		t.Fatalf("AgentToken under secret B: %q %v, want the token of secret B", token, err)
	}
	if err := withB.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("the token of secret A after the move: %v, want ErrUnauthenticated", err)
	}
}

// With the secret set, a stored token that is the generation-0 token of its
// agent ID, which an older version stored, becomes a derived credential when
// the agent syncs with it, in the transaction that checks it: the token is no
// longer stored, and the agent keeps working. Another stored token stays
// stored, and so does a derivable one while no secret is set.
func TestStoredTokenThatDerivesBecomesDerivedAtSync(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	created := time.Unix(1700000000, 0)
	for id, token := range map[string]string{"agent-1": tokenA1, "agent-2": "legacy-token"} {
		cred, err := model.NewStoredAgentCredential(id, token)
		if err != nil {
			t.Fatal(err)
		}
		cred.SetCreatedAt(created)
		if err := store.UpsertAgentCredential(ctx, cred); err != nil {
			t.Fatal(err)
		}
	}

	if err := New(store, logger).VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatal(err)
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || cred.Derived() {
		t.Fatalf("agent-1 checked with no secret: %v, want it stored still", err)
	}

	counted := &countingTx{Storage: store}
	withA := New(counted, logger, WithEnrollSecret(enrollSecretA))
	for _, id := range []string{"agent-1", "agent-2"} {
		token := map[string]string{"agent-1": tokenA1, "agent-2": "legacy-token"}[id]
		if err := withA.VerifyOrEnrollToken(ctx, id, token); err != nil {
			t.Fatalf("%s with its stored token: %v", id, err)
		}
	}
	if counted.txs != 2 {
		t.Fatalf("two checks took %d transactions, want 2", counted.txs)
	}
	upgraded, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil || !upgraded.Derived() || upgraded.Generation() != 0 || hex.EncodeToString(upgraded.SecretID()) != secretIDA || upgraded.Token() != "" {
		t.Fatalf("agent-1 after its sync: %v, want derived at 0 under %s with no token", err, secretIDA)
	}
	if !upgraded.CreatedAt().Equal(created) {
		t.Fatalf("agent-1's enrollment time moved: %v, want %v", upgraded.CreatedAt(), created)
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-2"); err != nil || cred.Derived() || cred.Token() != "legacy-token" {
		t.Fatalf("agent-2 after its sync: %v, want it stored with its token", err)
	}
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatalf("agent-1 syncing again: %v", err)
	}
}

// AgentToken gives the token podium presents to an agent: the stored token of a
// stored credential, the token derived for the generation of a derived one, and
// none for an agent without a credential. It writes nothing: a stored token
// that derives stays stored. With agent auth disabled podium presents no token,
// so it looks none up, and a derived credential with no secret is no error.
func TestAgentTokenOfEachKind(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	stored, err := model.NewStoredAgentCredential("agent-2", tokenA2)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := model.NewDerivedAgentCredential("agent-1", 1, SecretID(enrollSecretA))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*model.AgentCredential{stored, derived} {
		if err := store.UpsertAgentCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	for id, want := range map[string]string{"agent-1": tokenA1g1, "agent-2": tokenA2, "agent-3": ""} {
		if token, err := withA.AgentToken(ctx, id); err != nil || token != want {
			t.Errorf("AgentToken(%s): %q %v, want %q", id, token, err, want)
		}
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-2"); err != nil || cred.Derived() {
		t.Fatalf("AgentToken rewrote the stored credential of agent-2: %v", err)
	}
	disabled := New(store, logger, WithAuthDisabled())
	for _, id := range []string{"agent-1", "agent-2", "agent-3"} {
		if token, err := disabled.AgentToken(ctx, id); err != nil || token != "" {
			t.Errorf("AgentToken(%s) with agent auth disabled: %q %v, want no token and no error", id, token, err)
		}
	}
}

// The sweep converts, in one transaction, every stored credential whose token
// is the generation-0 token of its agent ID under the secret, keeping its
// enrollment time. It counts the stored credentials left and the derived ones
// podium cannot use: with another secret ID, or any while no secret is set. A
// sweep with nothing to convert writes nothing.
func TestSweepCredentials(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	created := time.Unix(1700000000, 0)
	put := func(c *model.AgentCredential, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		c.SetCreatedAt(created)
		if err := store.UpsertAgentCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	put(model.NewStoredAgentCredential("agent-1", tokenA1))
	put(model.NewStoredAgentCredential("agent-2", "legacy-token"))
	put(model.NewDerivedAgentCredential("agent-3", 0, SecretID(enrollSecretA)))
	put(model.NewDerivedAgentCredential("agent-4", 2, SecretID(enrollSecretB)))

	counted := &countingTx{Storage: store}
	withA := New(counted, logger, WithEnrollSecret(enrollSecretA))
	res, err := withA.SweepCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CredentialSweep{Converted: 1, Stored: 1, Unusable: 1}); res != want || counted.txs != 1 || counted.writes != 1 {
		t.Fatalf("sweep under secret A: %+v in %d transactions with %d writes, want %+v in one with one write", res, counted.txs, counted.writes, want)
	}
	cred, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil || !cred.Derived() || cred.Generation() != 0 || hex.EncodeToString(cred.SecretID()) != secretIDA || cred.Token() != "" || !cred.CreatedAt().Equal(created) {
		t.Fatalf("agent-1 after the sweep: %v, want derived at 0 under %s, no token, created %v", err, secretIDA, created)
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-2"); err != nil || cred.Derived() || cred.Token() != "legacy-token" {
		t.Fatalf("agent-2 after the sweep: %v, want it stored with its token", err)
	}

	counted.txs, counted.writes = 0, 0
	if res, err := withA.SweepCredentials(ctx); err != nil || res != (CredentialSweep{Stored: 1, Unusable: 1}) || counted.writes != 0 {
		t.Fatalf("second sweep: %+v %v with %d writes, want 1 stored and 1 unusable, and no write", res, err, counted.writes)
	}
	if res, err := New(store, logger).SweepCredentials(ctx); err != nil || res != (CredentialSweep{Stored: 1, Unusable: 3}) {
		t.Fatalf("sweep with no secret: %+v %v, want 1 stored and the 3 derived ones unusable", res, err)
	}
}

// countingTx counts the transactions run on a store and the credential writes
// made in them.
type countingTx struct {
	storage.Storage
	txs, writes int
}

func (c *countingTx) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	c.txs++
	return c.Storage.WithTx(ctx, func(tx storage.Storage) error {
		return fn(&countingWrites{Storage: tx, n: &c.writes})
	})
}

// countingWrites counts the credential writes made through a transaction.
type countingWrites struct {
	storage.Storage
	n *int
}

func (w *countingWrites) UpsertAgentCredential(ctx context.Context, c *model.AgentCredential) error {
	*w.n++
	return w.Storage.UpsertAgentCredential(ctx, c)
}
