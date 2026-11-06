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

// tokenA1g2 is the token of "agent-1" at generation 2 under secret A, and
// tokenA1Floor its token at the generation floorAt gives, floorSeconds, derived
// outside podium like the fixtures of token_test.go.
const (
	tokenA1g2    = "089a433310152c9aa06043ed2151343c55f47fa0da583cfcbd14d149070e51e0"
	tokenA1Floor = "733efc9f2aad156bbb305681bfc6e3c5d1fd2222d5253c4e72f6e7a5f79a588e"
)

// floorSeconds is the time of floorAt, in Unix seconds: the generation a reset
// gives a stored credential at that time while the secret is set.
const floorSeconds = 1762400000

// floorAt is the clock of the tests that reset a stored credential.
func floorAt() time.Time { return time.Unix(floorSeconds, 0) }

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
// transaction with one write, by a derived one under the current secret: a
// derived credential gets the next generation, and one of another secret moves
// to the current secret; a stored credential, which has no generation, gets the
// time of the reset in Unix seconds. The old token no longer verifies, the new
// one does and is the one podium presents, and the enrollment time stays.
func TestResetCredentialIssuesTheNextGeneration(t *testing.T) {
	ctx := context.Background()
	created := time.Unix(1700000000, 0)
	derived := func(generation uint32, secretID []byte) func() (*model.AgentCredential, error) {
		return func() (*model.AgentCredential, error) {
			return model.NewDerivedAgentCredential("agent-1", generation, secretID)
		}
	}
	for _, tc := range []struct {
		name       string
		row        func() (*model.AgentCredential, error)
		old        string // the token the agent holds
		generation uint32 // the generation of the new credential
		token      string // its token
	}{
		{"a stored credential gets the time of the reset", func() (*model.AgentCredential, error) {
			return model.NewStoredAgentCredential("agent-1", "legacy-token")
		}, "legacy-token", floorSeconds, tokenA1Floor},
		{"derived at generation 0", derived(0, SecretID(enrollSecretA)), tokenA1, 1, tokenA1g1},
		{"derived at generation 1", derived(1, SecretID(enrollSecretA)), tokenA1g1, 2, tokenA1g2},
		{"derived under another secret, at generation 0", derived(0, SecretID(enrollSecretB)), tokenB1, 1, tokenA1g1},
		{"derived under another secret, at generation 1", derived(1, SecretID(enrollSecretB)), tokenB1g1, 2, tokenA1g2},
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

// The generation a reset gives a stored credential is the time in Unix seconds,
// at least 1, so that a clock at or before the epoch gives no generation 0, the
// token the agent ID enrolls with, and below the last generation, so that the
// floor leaves a derived reset to come before ErrGenerationExhausted refuses
// the next one.
func TestResetOfAStoredCredentialTakesTheTimeFloor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		clock time.Time
		want  uint32
	}{
		{"now", floorAt(), floorSeconds},
		{"one second after the epoch", time.Unix(1, 0), 1},
		{"the epoch", time.Unix(0, 0), 1},
		{"before the epoch", time.Unix(-5, 0), 1},
		{"the zero time", time.Time{}, 1},
		{"the second before the last generation", time.Unix(math.MaxUint32-1, 0), math.MaxUint32 - 1},
		{"the second of the last generation", time.Unix(math.MaxUint32, 0), math.MaxUint32 - 1},
		{"past the last generation", time.Unix(1<<40, 0), math.MaxUint32 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := inmemory.New()
			putCredential(t, store, time.Unix(1700000000, 0))(model.NewStoredAgentCredential("agent-1", "legacy-token"))
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
	putCredential(t, store, time.Unix(1700000000, 0))(model.NewStoredAgentCredential("agent-1", "legacy-token"))
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
// generation 2 by two resets, its secret is removed and a reset gives it a stored
// credential, then the same secret is set again and it is reset once more: the
// new generation is the time floor, above 2, so the tokens of generations 1 and 2,
// which the resets revoked, stay refused, and so do the token it enrolled with and
// the stored one. Counting from 1 again would let the token of generation 1 back
// in.
func TestResetAfterAStoredInterludeRevivesNoGeneration(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	withA := New(store, logger, WithEnrollSecret(enrollSecretA))
	withA.clock = floorAt
	if err := withA.VerifyOrEnrollToken(ctx, "agent-1", tokenA1); err != nil {
		t.Fatalf("enroll under secret A: %v", err)
	}
	for _, step := range []struct {
		generation uint32
		token      string
	}{{1, tokenA1g1}, {2, tokenA1g2}} {
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
	issued, err := withA.ResetCredential(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (IssuedCredential{AgentID: "agent-1", Token: tokenA1Floor, Derived: true, Generation: floorSeconds}); issued != want {
		t.Fatalf("reset after the interlude: %+v, want %+v, above generation 2", issued, want)
	}
	for _, old := range []struct{ name, token string }{
		{"the enrollment token", tokenA1},
		{"the token of generation 1", tokenA1g1},
		{"the token of generation 2", tokenA1g2},
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
// credential of the agent and the execution history stay, and an agent without a
// row is not found, whatever else the store holds of it.
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
