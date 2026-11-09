package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"regexp"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// tokenA1g2 is the token of "agent-1" at generation 2 under secret A. The others
// are its tokens at floorSeconds (tokenA1Floor) and at the generation in the name,
// that many seconds before or after it (tokenA1After60 is floorSeconds+60). All
// are derived outside podium like the fixtures of token_test.go.
const (
	tokenA1g2 = "089a433310152c9aa06043ed2151343c55f47fa0da583cfcbd14d149070e51e0"

	tokenA1Before1   = "60909b8e8283941fa8ec16d435d762a8f8e7baf7c16e5f42d98597d2aa87c45c"
	tokenA1Floor     = "733efc9f2aad156bbb305681bfc6e3c5d1fd2222d5253c4e72f6e7a5f79a588e"
	tokenA1After1    = "45fd550a3db7cb506c92dafe7f0b22c54cb2f9e3f31d8b89281fe5d00babf618"
	tokenA1After5    = "50f09d34ae7ff42a390acdb804429307a32087e7e30ebab25993504985c6ecf9"
	tokenA1After6    = "e406edb1cf4994a4ff62d7fbf3f642af009d5ebf1514aec8f3c3ecc3285acc8c"
	tokenA1After60   = "3fac914f76ff10cc67406950558f9f2c62a023464517326ba47d3c396818df6a"
	tokenA1After120  = "54702c1c47db698c83de0a4d9da169140ea0fafee143fd7664c30de61487e534"
	tokenA1After3600 = "87ad18abbdc50664520e2b3b5c9212c43cb1b6f2a0cba88796fd568a83f1fec1"
)

// floorSeconds is the time of floorAt, in Unix seconds: the generation a reset
// gives at that time unless the generation after the old one is later.
const floorSeconds = 1762400000

// floorAt is the clock of the tests that reset a credential at one moment.
func floorAt() time.Time { return time.Unix(floorSeconds, 0) }

// storedRow and derivedRow return the constructors of the credentials of
// agent-1 that putCredential stores: a stored one with the token, a derived one
// at the generation under the secret with the ID secretID.
func storedRow(token string) func() (*model.AgentCredential, error) {
	return func() (*model.AgentCredential, error) { return model.NewStoredAgentCredential("agent-1", token) }
}

func derivedRow(generation uint32, secretID []byte) func() (*model.AgentCredential, error) {
	return func() (*model.AgentCredential, error) {
		return model.NewDerivedAgentCredential("agent-1", generation, secretID)
	}
}

// putCredential stores the credential c, built by the constructor that returned
// (c, err), with the enrollment time created.
func putCredential(t *testing.T, store storage.Storage, created time.Time) func(*model.AgentCredential, error) {
	t.Helper()
	return func(c *model.AgentCredential, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		c.SetCreatedAt(created)
		if err := store.UpsertAgentCredential(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
}

// With the enrollment secret set, a reset replaces the credential, in one
// transaction with one write, by a derived one under the current secret, at the
// later of the generation after the old one and the time of the reset in Unix
// seconds: a stored credential, which has no generation, counts as generation 0,
// and one of another secret moves to the current secret. The old token no longer
// verifies, the new one does and is the one podium presents, and the enrollment
// time stays.
func TestResetCredentialIssuesTheLaterOfTheNextGenerationAndTheTime(t *testing.T) {
	ctx := context.Background()
	created := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name       string
		row        func() (*model.AgentCredential, error)
		old        string // the token the agent holds
		generation uint32 // the generation of the new credential
		token      string // its token
	}{
		{"a stored credential", storedRow("legacy-token"), "legacy-token", floorSeconds, tokenA1Floor},
		{"derived at generation 0", derivedRow(0, SecretID(enrollSecretA)), tokenA1, floorSeconds, tokenA1Floor},
		{"derived at generation 1", derivedRow(1, SecretID(enrollSecretA)), tokenA1g1, floorSeconds, tokenA1Floor},
		{"derived under another secret, at generation 0", derivedRow(0, SecretID(enrollSecretB)), tokenB1, floorSeconds, tokenA1Floor},
		{"derived under another secret, at generation 1", derivedRow(1, SecretID(enrollSecretB)), tokenB1g1, floorSeconds, tokenA1Floor},
		{"derived at the generation before the time", derivedRow(floorSeconds-1, SecretID(enrollSecretA)), tokenA1Before1, floorSeconds, tokenA1Floor},
		{"derived at the time", derivedRow(floorSeconds, SecretID(enrollSecretA)), tokenA1Floor, floorSeconds + 1, tokenA1After1},
		{"derived after the time", derivedRow(floorSeconds+5, SecretID(enrollSecretA)), tokenA1After5, floorSeconds + 6, tokenA1After6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			putCredential(t, store, created)(tc.row())
			counted := &countingTx{Storage: store}
			svc := New(counted, zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
			svc.clock = floorAt

			issued, err := svc.ResetCredential(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if want := (IssuedCredential{AgentID: "agent-1", Token: tc.token, Derived: true, Generation: tc.generation}); issued != want {
				t.Fatalf("issued %+v, want %+v", issued, want)
			}
			if counted.txs != 1 || counted.writes != 1 {
				t.Fatalf("the reset took %d transactions and %d credential writes, want one of each", counted.txs, counted.writes)
			}
			cred, err := store.GetAgentCredential(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if !cred.Derived() || cred.Generation() != tc.generation || hex.EncodeToString(cred.SecretID()) != secretIDA || cred.Token() != "" {
				t.Fatalf("credential after the reset: derived %v, generation %d, secret ID %x, %d-byte token; want derived at %d under %s with no token",
					cred.Derived(), cred.Generation(), cred.SecretID(), len(cred.Token()), tc.generation, secretIDA)
			}
			if !cred.CreatedAt().Equal(created) {
				t.Fatalf("the enrollment time moved: %v, want %v", cred.CreatedAt(), created)
			}
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tc.old); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("the old token after the reset: %v, want ErrUnauthenticated", err)
			}
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tc.token); err != nil {
				t.Fatalf("the new token: %v", err)
			}
			if token, err := svc.AgentToken(ctx, "agent-1"); err != nil || token != tc.token {
				t.Fatalf("AgentToken after the reset: %q %v, want the new token", token, err)
			}
		})
	}
}

// The floor of a reset is the time in Unix seconds, at least 1, so that a clock
// at or before the epoch gives no generation 0, the token the agent ID enrolls
// with, and below the last generation, so that the floor leaves a reset to come
// before ErrGenerationExhausted refuses the next one. The generation after the
// old one wins when it is later than the floor.
func TestResetTakesTheTimeFloor(t *testing.T) {
	ctx := context.Background()
	secretA := SecretID(enrollSecretA)
	for _, tc := range []struct {
		name  string
		row   func() (*model.AgentCredential, error)
		clock time.Time
		want  uint32
	}{
		{"stored, now", storedRow("legacy-token"), floorAt(), floorSeconds},
		{"stored, one second after the epoch", storedRow("legacy-token"), time.Unix(1, 0), 1},
		{"stored, the epoch", storedRow("legacy-token"), time.Unix(0, 0), 1},
		{"stored, before the epoch", storedRow("legacy-token"), time.Unix(-5, 0), 1},
		{"stored, the zero time", storedRow("legacy-token"), time.Time{}, 1},
		{"stored, the second before the last generation", storedRow("legacy-token"), time.Unix(math.MaxUint32-1, 0), math.MaxUint32 - 1},
		{"stored, the second of the last generation", storedRow("legacy-token"), time.Unix(math.MaxUint32, 0), math.MaxUint32 - 1},
		{"stored, past the last generation", storedRow("legacy-token"), time.Unix(1<<40, 0), math.MaxUint32 - 1},
		{"derived at generation 7, the epoch", derivedRow(7, secretA), time.Unix(0, 0), 8},
		{"derived at generation 7, before the epoch", derivedRow(7, secretA), time.Unix(-5, 0), 8},
		{"derived at generation 7, the zero time", derivedRow(7, secretA), time.Time{}, 8},
		{"derived at generation 7, past the last generation", derivedRow(7, secretA), time.Unix(1<<40, 0), math.MaxUint32 - 1},
		{"derived at the generation before the last, past it", derivedRow(math.MaxUint32-1, secretA), time.Unix(1<<40, 0), math.MaxUint32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			putCredential(t, store, time.Unix(1700000000, 0))(tc.row())
			svc := New(store, zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
			svc.clock = func() time.Time { return tc.clock }

			issued, err := svc.ResetCredential(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if want := (IssuedCredential{AgentID: "agent-1", Token: DeriveToken(enrollSecretA, "agent-1", tc.want), Derived: true, Generation: tc.want}); issued != want {
				t.Fatalf("issued %+v, want %+v", issued, want)
			}
			if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || !cred.Derived() || cred.Generation() != tc.want {
				t.Fatalf("credential after the reset: %v, want derived at %d", err, tc.want)
			}
		})
	}

	// The top stays guarded: a floor at the generation before the last leaves one
	// reset, and the one after it is refused.
	store := inmemory.New()
	putCredential(t, store, time.Unix(1700000000, 0))(storedRow("legacy-token")())
	svc := New(store, zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
	svc.clock = func() time.Time { return time.Unix(1<<40, 0) }
	for _, want := range []uint32{math.MaxUint32 - 1, math.MaxUint32} {
		if issued, err := svc.ResetCredential(ctx, "agent-1"); err != nil || issued.Generation != want {
			t.Fatalf("reset: %+v %v, want generation %d", issued, err, want)
		}
	}
	if _, err := svc.ResetCredential(ctx, "agent-1"); !errors.Is(err, ErrGenerationExhausted) {
		t.Fatalf("reset at the last generation: %v, want ErrGenerationExhausted", err)
	}
}

// A stored interlude brings no revoked generation back. The agent is derived at
// a generation by two resets, its secret is removed and a reset gives it a stored
// credential, then the same secret is set again and it is reset once more: the
// new generation is the time of that reset, above the two before the interlude,
// so their tokens, which the resets revoked, stay refused, and so do the token it
// enrolled with and the stored one. A stored credential counts as generation 0,
// so without the time the reset would issue generation 1, below the two.
func TestResetAfterAStoredInterludeRevivesNoGeneration(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	now := floorAt()
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	withA.clock = func() time.Time { return now }
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatalf("enroll under secret A: %v", err)
	}
	for _, step := range []struct {
		after      time.Duration // the time of the reset, after floorAt
		generation uint32
		token      string
	}{{0, floorSeconds, tokenA1Floor}, {time.Minute, floorSeconds + 60, tokenA1After60}} {
		now = floorAt().Add(step.after)
		issued, err := withA.ResetCredential(ctx, "agent-1")
		if err != nil || issued.Generation != step.generation || issued.Token != step.token {
			t.Fatalf("reset under secret A: %+v %v, want generation %d", issued, err, step.generation)
		}
		if err := withA.VerifyOrEnrollToken(ctx, "agent-1", step.token); err != nil {
			t.Fatalf("sync at generation %d: %v", step.generation, err)
		}
	}

	// The secret is removed: the reset stores a random token, which lets the agent in.
	without := New(store, logger)
	stored, err := without.ResetCredential(ctx, "agent-1")
	if err != nil || stored.Derived {
		t.Fatalf("reset with no secret: %+v %v, want a stored credential", stored, err)
	}
	if err := without.VerifyOrEnrollToken(ctx, "agent-1", stored.Token); err != nil {
		t.Fatalf("sync with the stored token: %v", err)
	}

	// The same secret is set again, and the stored credential is reset.
	now = floorAt().Add(time.Hour)
	issued, err := withA.ResetCredential(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (IssuedCredential{AgentID: "agent-1", Token: tokenA1After3600, Derived: true, Generation: floorSeconds + 3600}); issued != want {
		t.Fatalf("reset after the interlude: %+v, want %+v, above generation %d", issued, want, floorSeconds+60)
	}
	for _, old := range []struct{ name, token string }{
		{"the enrollment token", tokenA1},
		{"the token of the first reset", tokenA1Floor},
		{"the token of the second reset", tokenA1After60},
		{"the stored token", stored.Token},
	} {
		if err := withA.VerifyOrEnrollToken(ctx, "agent-1", old.token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s after the interlude: %v, want ErrUnauthenticated", old.name, err)
		}
	}
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", issued.Token); err != nil {
		t.Fatalf("the new token: %v", err)
	}
	if token, err := withA.AgentToken(ctx, "agent-1"); err != nil || token != issued.Token {
		t.Fatalf("AgentToken after the interlude: %q %v, want the new token", token, err)
	}
}

// Restoring a backup brings the credential back at the generation g it had when
// the backup was taken, though it was reset twice since, to g1 and g2. One reset
// after the restore is enough: its generation is above g2, so the tokens of g,
// g1 and g2 are all refused. A reset that only counted up from g would issue g+1,
// which is g1 when the resets since counted up too. The resets here come a minute
// and an hour apart; the guarantee holds while each falls in a later second than
// the one before it.
func TestResetAfterARestoredBackupExceedsEveryGenerationIssued(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	now := floorAt()
	svc := New(store, zerolog.New(io.Discard), WithEnrollSecret(enrollSecretA))
	svc.clock = func() time.Time { return now }
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatalf("enroll under secret A: %v", err)
	}
	resetAfter := func(after time.Duration) IssuedCredential {
		t.Helper()
		now = floorAt().Add(after)
		issued, err := svc.ResetCredential(ctx, "agent-1")
		if err != nil {
			t.Fatal(err)
		}
		return issued
	}

	g := resetAfter(0)
	backup, err := store.GetAgentCredential(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	g1 := resetAfter(time.Minute)
	g2 := resetAfter(2 * time.Minute)
	for _, step := range []struct {
		name   string
		issued IssuedCredential
		want   IssuedCredential
	}{
		{"g", g, IssuedCredential{AgentID: "agent-1", Token: tokenA1Floor, Derived: true, Generation: floorSeconds}},
		{"g1", g1, IssuedCredential{AgentID: "agent-1", Token: tokenA1After60, Derived: true, Generation: floorSeconds + 60}},
		{"g2", g2, IssuedCredential{AgentID: "agent-1", Token: tokenA1After120, Derived: true, Generation: floorSeconds + 120}},
	} {
		if step.issued != step.want {
			t.Errorf("reset to %s: %+v, want %+v", step.name, step.issued, step.want)
		}
	}

	// The backup is restored: the credential is back at g, which lets g in.
	if err := store.UpsertAgentCredential(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", g.Token); err != nil {
		t.Fatalf("the token of g after the restore: %v, want the restored credential to accept it", err)
	}

	issued := resetAfter(time.Hour)
	if want := (IssuedCredential{AgentID: "agent-1", Token: tokenA1After3600, Derived: true, Generation: floorSeconds + 3600}); issued != want {
		t.Errorf("reset after the restore: %+v, want %+v", issued, want)
	}
	if issued.Generation <= g2.Generation {
		t.Errorf("the reset after the restore issued generation %d, want one above %d, the latest issued", issued.Generation, g2.Generation)
	}
	for _, old := range []struct {
		name   string
		issued IssuedCredential
	}{{"g", g}, {"g1", g1}, {"g2", g2}} {
		if err := svc.VerifyOrEnrollToken(ctx, "agent-1", old.issued.Token); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("the token of %s after the reset: %v, want ErrUnauthenticated", old.name, err)
		}
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", issued.Token); err != nil {
		t.Errorf("the new token: %v", err)
	}
	if token, err := svc.AgentToken(ctx, "agent-1"); err != nil || token != issued.Token {
		t.Errorf("AgentToken after the reset: %q %v, want the new token", token, err)
	}
}

// Without a secret a reset stores a new random token of 32 bytes, as lowercase
// hex, which nothing derives again: the old token no longer verifies, the new
// one does and is the one podium presents, a second reset issues another, and
// the enrollment time stays. This holds for a derived credential whose secret
// was removed too: the reset gives its agent a way back.
func TestResetCredentialWithoutSecretStoresARandomToken(t *testing.T) {
	ctx := context.Background()
	created := time.Unix(1700000000, 0)
	hex32 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, tc := range []struct {
		name string
		row  func() (*model.AgentCredential, error)
		old  string
	}{
		{"a stored credential", func() (*model.AgentCredential, error) {
			return model.NewStoredAgentCredential("agent-1", "chosen-token")
		}, "chosen-token"},
		{"a derived credential of a secret that was removed", func() (*model.AgentCredential, error) {
			return model.NewDerivedAgentCredential("agent-1", 2, SecretID(enrollSecretA))
		}, tokenA1g2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			putCredential(t, store, created)(tc.row())
			counted := &countingTx{Storage: store}
			svc := New(counted, zerolog.New(io.Discard))

			first, err := svc.ResetCredential(ctx, "agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if first.AgentID != "agent-1" || first.Derived || first.Generation != 0 || !hex32.MatchString(first.Token) {
				t.Fatalf("issued %+v, want a stored credential with a 64-digit lowercase hex token and no generation", first)
			}
			if counted.txs != 1 || counted.writes != 1 {
				t.Fatalf("the reset took %d transactions and %d credential writes, want one of each", counted.txs, counted.writes)
			}
			cred, err := store.GetAgentCredential(ctx, "agent-1")
			if err != nil || cred.Derived() || cred.Token() != first.Token || cred.Generation() != 0 || cred.SecretID() != nil {
				t.Fatalf("credential after the reset: %v, want a stored one that holds the new token", err)
			}
			if !cred.CreatedAt().Equal(created) {
				t.Fatalf("the enrollment time moved: %v, want %v", cred.CreatedAt(), created)
			}
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", tc.old); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("the old token after the reset: %v, want ErrUnauthenticated", err)
			}
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", first.Token); err != nil {
				t.Fatalf("the new token: %v", err)
			}
			if token, err := svc.AgentToken(ctx, "agent-1"); err != nil || token != first.Token {
				t.Fatalf("AgentToken after the reset: %q %v, want the new token", token, err)
			}

			second, err := svc.ResetCredential(ctx, "agent-1")
			if err != nil || second.Token == first.Token || !hex32.MatchString(second.Token) {
				t.Fatalf("second reset: %+v %v, want another random token", second, err)
			}
			if err := svc.VerifyOrEnrollToken(ctx, "agent-1", first.Token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("the first new token after a second reset: %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// A reset changes nothing and returns no token when there is nothing to reset: an
// agent without a credential is not found, an empty ID is invalid, agent
// authentication is off (podium checks and presents no token, so there is no
// credential to reset), and a credential at the last generation has no next one
// that would not repeat a token: the generation does not wrap around to 0, the
// token the agent ID enrolls with.
func TestResetCredentialRefusals(t *testing.T) {
	ctx := context.Background()
	created := time.Unix(1700000000, 0)
	logger := zerolog.New(io.Discard)
	for _, tc := range []struct {
		name string
		id   string
		row  func() (*model.AgentCredential, error)
		opts []Option
		want error
		kind errkind.Kind
	}{
		{"an agent with no credential", "agent-1", nil, []Option{WithEnrollSecret(enrollSecretA)}, storage.ErrNotFound, errkind.NotFound},
		{"an empty ID", "", nil, []Option{WithEnrollSecret(enrollSecretA)}, storage.ErrInvalidArgument, errkind.InvalidArgument},
		{"agent authentication off, a stored credential", "agent-1", func() (*model.AgentCredential, error) {
			return model.NewStoredAgentCredential("agent-1", "chosen-token")
		}, []Option{WithAuthDisabled()}, ErrAuthDisabled, errkind.FailedPrecondition},
		{"agent authentication off, a derived credential", "agent-1", func() (*model.AgentCredential, error) {
			return model.NewDerivedAgentCredential("agent-1", 1, SecretID(enrollSecretA))
		}, []Option{WithAuthDisabled()}, ErrAuthDisabled, errkind.FailedPrecondition},
		{"the last generation", "agent-1", func() (*model.AgentCredential, error) {
			return model.NewDerivedAgentCredential("agent-1", math.MaxUint32, SecretID(enrollSecretA))
		}, []Option{WithEnrollSecret(enrollSecretA)}, ErrGenerationExhausted, errkind.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			if tc.row != nil {
				putCredential(t, store, created)(tc.row())
			}
			before, beforeErr := store.GetAgentCredential(ctx, "agent-1")
			counted := &countingTx{Storage: store}
			svc := New(counted, logger, tc.opts...)

			issued, err := svc.ResetCredential(ctx, tc.id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ResetCredential: %v, want %v", err, tc.want)
			}
			if issued != (IssuedCredential{}) {
				t.Fatalf("a refused reset issued %+v", issued)
			}
			if kind := errkind.Classify(err); kind != tc.kind {
				t.Fatalf("the error classifies as %v, want %v", kind, tc.kind)
			}
			if counted.writes != 0 {
				t.Fatalf("a refused reset wrote %d credentials", counted.writes)
			}
			after, afterErr := store.GetAgentCredential(ctx, "agent-1")
			switch {
			case beforeErr != nil:
				if !errors.Is(afterErr, storage.ErrNotFound) {
					t.Fatalf("a refused reset stored a credential: %v", afterErr)
				}
			case afterErr != nil || after.Derived() != before.Derived() || after.Generation() != before.Generation() || after.Token() != before.Token() ||
				hex.EncodeToString(after.SecretID()) != hex.EncodeToString(before.SecretID()) || !after.UpdatedAt().Equal(before.UpdatedAt()):
				t.Fatalf("a refused reset changed the credential: %v", afterErr)
			}
		})
	}
}

// failingDelete fails the delete of an agent row or of a rollout inside the
// transactions of the store it wraps, after the writes that came before it ran.
type failingDelete struct {
	storage.Storage
	agent, rollout bool
}

func (f *failingDelete) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	return f.Storage.WithTx(ctx, func(tx storage.Storage) error {
		return fn(&failingDeleteTx{Storage: tx, agent: f.agent, rollout: f.rollout})
	})
}

type failingDeleteTx struct {
	storage.Storage
	agent, rollout bool
}

var errInjected = errors.New("injected failure")

func (f *failingDeleteTx) DeleteAgent(ctx context.Context, id string) error {
	if f.agent {
		return errInjected
	}
	return f.Storage.DeleteAgent(ctx, id)
}

func (f *failingDeleteTx) DeleteRollout(ctx context.Context, id string) error {
	if f.rollout {
		return errInjected
	}
	return f.Storage.DeleteRollout(ctx, id)
}

// seedAgentWithRollouts stores two agents, a rollout of agent-1 for each of two
// specs and one of agent-2, the credential of agent-1 and an execution record of
// each agent.
func seedAgentWithRollouts(t *testing.T, store storage.Storage) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"agent-1", "agent-2"} {
		a, err := model.NewAgent(id, "name of "+id, "http://10.0.0.5:8085")
		if err != nil {
			t.Fatal(err)
		}
		if err = store.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		rec, err := model.NewExecutionRecord(model.ExecutionRecordParams{
			AgentID: id, TaskName: "task", TaskUID: "uid-" + id, Generation: 1, Attempt: 1, Status: "succeeded", CapturedAt: time.Unix(1700000000, 0),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = store.UpsertExecution(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{"spec-a", "agent-1"}, {"spec-b", "agent-1"}, {"spec-a", "agent-2"}} {
		ro, err := model.NewRollout(pair[0], pair[1], 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.UpsertRollout(ctx, ro); err != nil {
			t.Fatal(err)
		}
	}
	putCredential(t, store, time.Unix(1700000000, 0))(model.NewStoredAgentCredential("agent-1", "chosen-token"))
}

// holds returns the IDs of the rollouts the store holds, sorted by the store.
func holds(t *testing.T, store storage.Storage) []string {
	t.Helper()
	all, err := storage.AllRollouts(context.Background(), store, store.BuildRolloutFilter(storage.RolloutQueryCriteria{}))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(all))
	for _, ro := range all {
		ids = append(ids, ro.ID())
	}
	return ids
}

// Deleting an agent removes its row and every rollout of the agent in one
// transaction, and returns the row it removed. The rollouts of other agents, the
// credential of the agent and the execution history stay, and a delete that finds
// neither a row nor a rollout of the ID is not found.
func TestDeleteRemovesTheRowAndTheRolloutsOfTheAgent(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	seedAgentWithRollouts(t, store)
	counted := &countingTx{Storage: store}
	svc := New(counted, zerolog.New(io.Discard))

	removed, err := svc.Delete(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if removed == nil || removed.ID() != "agent-1" || removed.Name() != "name of agent-1" {
		t.Fatalf("Delete returned %v, want the row of agent-1", removed)
	}
	if counted.txs != 1 {
		t.Fatalf("the delete took %d transactions, want 1", counted.txs)
	}
	if _, err := store.GetAgent(ctx, "agent-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("agent-1 after the delete: %v, want not found", err)
	}
	if got, want := holds(t, store), []string{model.RolloutID("spec-a", "agent-2")}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("rollouts after the delete: %v, want only %v", got, want)
	}
	if _, err := store.GetAgent(ctx, "agent-2"); err != nil {
		t.Fatalf("agent-2 after the delete of agent-1: %v", err)
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || cred.Token() != "chosen-token" {
		t.Fatalf("credential of agent-1 after the delete: %v, want it kept", err)
	}
	for _, id := range []string{"agent-1", "agent-2"} {
		records, err := storage.AllExecutions(ctx, store, store.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: id}))
		if err != nil || len(records) != 1 {
			t.Fatalf("execution history of %s after the delete: %d records %v, want 1", id, len(records), err)
		}
	}

	if _, err := svc.Delete(ctx, "agent-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("second delete: %v, want not found", err)
	}
	if _, err := svc.Delete(ctx, "unknown"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("delete of an unknown agent: %v, want not found", err)
	}
	if _, err := svc.Delete(ctx, ""); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("delete of an empty ID: %v, want ErrInvalidArgument", err)
	}
	if len(holds(t, store)) != 1 {
		t.Fatalf("a refused delete changed the rollouts: %v", holds(t, store))
	}
}

// An agent that lifecycle deleted has no row and keeps its rollouts: its delete
// drops them in one transaction and returns no row. The rollouts of other agents
// and the credential stay, and a second delete, which finds nothing, is not found.
func TestDeleteDropsTheRolloutsOfAnAgentWithoutARow(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	seedAgentWithRollouts(t, store)
	if err := store.DeleteAgent(ctx, "agent-1"); err != nil {
		t.Fatal(err)
	}
	counted := &countingTx{Storage: store}
	svc := New(counted, zerolog.New(io.Discard))

	removed, err := svc.Delete(ctx, "agent-1")
	if err != nil {
		t.Fatalf("delete of the rollouts of an agent without a row: %v", err)
	}
	if removed != nil {
		t.Fatalf("Delete returned the row %v, want none", removed)
	}
	if counted.txs != 1 {
		t.Fatalf("the delete took %d transactions, want 1", counted.txs)
	}
	if got, want := holds(t, store), []string{model.RolloutID("spec-a", "agent-2")}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("rollouts after the delete: %v, want only %v", got, want)
	}
	if cred, err := store.GetAgentCredential(ctx, "agent-1"); err != nil || cred.Token() != "chosen-token" {
		t.Fatalf("credential of agent-1 after the delete: %v, want it kept", err)
	}
	if _, err := svc.Delete(ctx, "agent-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("second delete: %v, want not found", err)
	}
}

// The row and the rollouts go in one transaction: when either delete fails, the
// other is rolled back, so an agent never loses its rollouts and keeps its row,
// or the reverse.
func TestDeleteIsAtomic(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fail failingDelete
	}{
		{"the agent row", failingDelete{agent: true}},
		{"a rollout", failingDelete{rollout: true}},
	} {
		t.Run("fails on "+tc.name, func(t *testing.T) {
			store := inmemory.New()
			seedAgentWithRollouts(t, store)
			tc.fail.Storage = store
			svc := New(&tc.fail, zerolog.New(io.Discard))

			if _, err := svc.Delete(ctx, "agent-1"); !errors.Is(err, errInjected) {
				t.Fatalf("Delete: %v, want the injected failure", err)
			}
			if _, err := store.GetAgent(ctx, "agent-1"); err != nil {
				t.Fatalf("agent-1 after the failed delete: %v, want it kept", err)
			}
			if got := holds(t, store); len(got) != 3 {
				t.Fatalf("rollouts after the failed delete: %v, want all 3", got)
			}
		})
	}
}
