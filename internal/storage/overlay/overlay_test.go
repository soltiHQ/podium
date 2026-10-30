package overlay_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

func seeded(t *testing.T) *inmemory.Store {
	t.Helper()
	base := inmemory.New()
	if err := base.WithTx(t.Context(), func(tx storage.Storage) error { return seed(t, tx) }); err != nil {
		t.Fatal(err)
	}
	return base
}

// TestOverlayRefusesWhatApplyRefuses makes, for each kind of check, a write that the
// state machine would refuse to apply. The overlay refuses it in the transaction
// instead, with the error the stores give, records nothing and leaves the row as it
// was; and ApplyOps does refuse the operation the write would have recorded, so the
// check is the one the state machine makes.
func TestOverlayRefusesWhatApplyRefuses(t *testing.T) {
	ctx := t.Context()
	base := seeded(t)

	undated := agent(t, "agent-a", 1, "prod")
	undated.SetCreatedAt(time.Time{})
	anonymous := user(t, "user-a", "sub-a", 1)
	anonymous.SetSubject("")
	undatedRollout := rollout(t, "spec-d", "agent-d", 1)
	undatedRollout.SetCreatedAt(time.Time{})
	unencodable := spec(t, "spec-a", 1)
	unencodable.SetKindConfig(map[string]any{"channel": make(chan int)})
	again := session(t, "sess-b", "user-b", "cred-b", 3)
	if err := again.RotateRefreshAt([]byte("replaced"), minute(9), minute(4)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		write     func(tx storage.Storage) error
		want      error
		op        *raftv1.Op // what the write would record; nil when it has no wire form
		unchanged func(tx storage.Storage) error
	}{
		{
			"agent without a creation time", func(tx storage.Storage) error { return tx.UpsertAgent(ctx, undated) },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_AgentUpsert{AgentUpsert: wire.AgentToProto(undated)}},
			func(tx storage.Storage) error { return missing(tx.GetAgent(ctx, "agent-a")) },
		},
		{
			"user without a subject", func(tx storage.Storage) error { return tx.UpsertUser(ctx, anonymous) },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_UserUpsert{UserUpsert: wire.UserToProto(anonymous)}},
			func(tx storage.Storage) error { return missing(tx.GetUser(ctx, "user-a")) },
		},
		{
			"rollout without a creation time", func(tx storage.Storage) error { return tx.UpsertRollout(ctx, undatedRollout) },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_RolloutUpsert{RolloutUpsert: wire.RolloutToProto(undatedRollout)}},
			func(tx storage.Storage) error { return missing(tx.GetRollout(ctx, undatedRollout.ID())) },
		},
		{
			"spec with a config that has no wire form", func(tx storage.Storage) error { return tx.UpsertSpec(ctx, unencodable) },
			storage.ErrInvalidArgument, nil,
			func(tx storage.Storage) error { return missing(tx.GetSpec(ctx, "spec-a")) },
		},
		{
			"session created twice", func(tx storage.Storage) error { return tx.CreateSession(ctx, again) },
			storage.ErrAlreadyExists, &raftv1.Op{Op: &raftv1.Op_SessionCreate{SessionCreate: wire.SessionToProto(again)}},
			func(tx storage.Storage) error {
				s, err := tx.GetSession(ctx, "sess-b")
				if err != nil {
					return err
				}
				return field("refresh hash", string(s.RefreshHash()), "hash of sess-b")
			},
		},
		{
			"rotation of a missing session", func(tx storage.Storage) error {
				return tx.RotateRefresh(ctx, "missing", []byte("hash"), minute(9))
			}, storage.ErrNotFound, overlay.RotateRefreshOp("missing", []byte("hash"), minute(9), minute(4)),
			func(tx storage.Storage) error { return missing(tx.GetSession(ctx, "missing")) },
		},
		{
			"revocation of a missing session", func(tx storage.Storage) error { return tx.RevokeSession(ctx, "missing", minute(9)) },
			storage.ErrNotFound, &raftv1.Op{Op: &raftv1.Op_SessionRevoke{SessionRevoke: &raftv1.SessionRevokeMsg{Id: "missing", RevokedAtNs: minute(9).UnixNano()}}},
			func(tx storage.Storage) error { return missing(tx.GetSession(ctx, "missing")) },
		},
		{
			"delete of a missing agent", func(tx storage.Storage) error { return tx.DeleteAgent(ctx, "missing") },
			storage.ErrNotFound, &raftv1.Op{Op: &raftv1.Op_AgentDelete{AgentDelete: "missing"}},
			func(tx storage.Storage) error { return missing(tx.GetAgent(ctx, "missing")) },
		},
		{
			"sessions of no user", func(tx storage.Storage) error { return tx.DeleteSessionsByUser(ctx, "") },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_SessionDeleteByUser{SessionDeleteByUser: ""}},
			func(tx storage.Storage) error { return present(tx.GetSession(ctx, "sess-b")) },
		},
		{
			"verifiers of no credential", func(tx storage.Storage) error { return tx.DeleteVerifierByCredential(ctx, "") },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_VerifierDeleteByCred{VerifierDeleteByCred: ""}},
			func(tx storage.Storage) error { return present(tx.GetVerifier(ctx, "ver-b")) },
		},
		{
			"rollouts of no spec", func(tx storage.Storage) error { return tx.DeleteRolloutsBySpec(ctx, "") },
			storage.ErrInvalidArgument, &raftv1.Op{Op: &raftv1.Op_RolloutDeleteBySpec{RolloutDeleteBySpec: ""}},
			func(tx storage.Storage) error {
				return present(tx.GetRollout(ctx, model.RolloutID("spec-b", "agent-b")))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := overlay.New(base)
			if err := tc.write(tx); !errors.Is(err, tc.want) {
				t.Fatalf("the write returned %v, want %v", err, tc.want)
			}
			if !tx.Empty() {
				t.Fatalf("the refused write recorded %d operations", len(tx.Ops()))
			}
			if err := tc.unchanged(tx); err != nil {
				t.Fatalf("the refused write changed what the transaction reads: %v", err)
			}
			if tc.op == nil {
				return
			}
			err := base.WithTx(ctx, func(dst storage.Storage) error { return overlay.ApplyOps(ctx, dst, []*raftv1.Op{tc.op}) })
			if err == nil {
				t.Fatal("ApplyOps applied the operation: the overlay refused a write the state machine takes")
			}
		})
	}
}

// TestOverlayBulkDeleteOfNothingRecordsNothing runs, for each bulk delete, a
// transaction whose only call is one that matches no row, committed or written:
// it records nothing, so Empty tells a transaction that changes nothing. A bulk
// delete that matches only a row the transaction wrote still records its operation.
func TestOverlayBulkDeleteOfNothingRecordsNothing(t *testing.T) {
	ctx := t.Context()
	base := seeded(t)
	for _, tc := range []struct {
		name  string
		bulk  func(tx storage.Storage) error // matches no committed row
		write func(tx storage.Storage) error // writes a row bulk matches
	}{
		{
			"DeleteSessionsByUser",
			func(tx storage.Storage) error { return tx.DeleteSessionsByUser(ctx, "user-f") },
			func(tx storage.Storage) error {
				return tx.CreateSession(ctx, session(t, "sess-a", "user-f", "cred-b", 1))
			},
		},
		{
			"DeleteVerifierByCredential",
			func(tx storage.Storage) error { return tx.DeleteVerifierByCredential(ctx, "cred-d") },
			func(tx storage.Storage) error { return tx.UpsertVerifier(ctx, verifier(t, "ver-d", "cred-d", 1)) },
		},
		{
			"DeleteRolloutsBySpec",
			func(tx storage.Storage) error { return tx.DeleteRolloutsBySpec(ctx, "spec-f") },
			func(tx storage.Storage) error { return tx.UpsertRollout(ctx, rollout(t, "spec-f", "agent-a", 1)) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := overlay.New(base)
			if err := tc.bulk(tx); err != nil {
				t.Fatal(err)
			}
			if !tx.Empty() || len(tx.Ops()) != 0 {
				t.Fatalf("a bulk delete that matches nothing recorded %d operations", len(tx.Ops()))
			}
			own := overlay.New(base)
			if err := errors.Join(tc.write(own), tc.bulk(own)); err != nil {
				t.Fatal(err)
			}
			if n := len(own.Ops()); n != 2 {
				t.Fatalf("a bulk delete of a row the transaction wrote left %d operations, want the write and the delete", n)
			}
		})
	}
}

// TestOverlayOpsReplayToWhatItRead commits a transaction that writes every kind of
// row as the state machine does, by applying its operations to the base, and then
// reads from the base what the transaction read. The operations are those of the
// writes that changed something, one each, in the order of the writes.
func TestOverlayOpsReplayToWhatItRead(t *testing.T) {
	ctx := t.Context()
	base := seeded(t)
	tx := overlay.New(base)

	b := agent(t, "agent-b", 3, "prod")
	b.SetOS("plan9")
	rewrite := execution(t, "b", 3).Params()
	rewrite.Status = "failed"
	rewritten, err := model.NewExecutionRecord(rewrite)
	if err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		op    string // the operation the write records, "" for none
		write func() error
	}{
		{"AgentUpsert", func() error { return tx.UpsertAgent(ctx, agent(t, "agent-a", 1, "dev")) }},
		{"AgentUpsert", func() error { return tx.UpsertAgent(ctx, b) }},
		{"AgentDelete", func() error { return tx.DeleteAgent(ctx, "agent-f") }},
		{"", func() error { return ignoreNotFound(tx.DeleteAgent(ctx, "agent-f")) }},
		{"AgentCredentialUpsert", func() error {
			return tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-d", "token-d", 1))
		}},
		{"AgentCredentialDelete", func() error { return tx.DeleteAgentCredential(ctx, "agent-b") }},
		{"UserUpsert", func() error { return tx.UpsertUser(ctx, user(t, "user-a", "sub-a", 1)) }},
		{"UserDelete", func() error { return tx.DeleteUser(ctx, "user-f") }},
		{"RoleUpsert", func() error { return tx.UpsertRole(ctx, role(t, "role-a", "admin", 1)) }},
		{"RoleDelete", func() error { return tx.DeleteRole(ctx, "role-f") }},
		{"CredentialUpsert", func() error {
			return tx.UpsertCredential(ctx, credential(t, "cred-a", "user-d", "cert", 1))
		}},
		{"CredentialDelete", func() error { return tx.DeleteCredential(ctx, "cred-f") }},
		{"VerifierUpsert", func() error { return tx.UpsertVerifier(ctx, verifier(t, "ver-d", "cred-d", 1)) }},
		{"VerifierDelete", func() error { return tx.DeleteVerifier(ctx, "ver-g") }},
		{"VerifierDeleteByCred", func() error { return tx.DeleteVerifierByCredential(ctx, "cred-b") }},
		{"SessionCreate", func() error { return tx.CreateSession(ctx, session(t, "sess-a", "user-b", "cred-b", 1)) }},
		{"SessionRotateRefresh", func() error {
			return tx.RotateRefresh(ctx, "sess-d", []byte("rotated"), minute(1).Add(48*time.Hour))
		}},
		{"SessionRevoke", func() error { return tx.RevokeSession(ctx, "sess-g", minute(5)) }},
		{"SessionDelete", func() error { return tx.DeleteSession(ctx, "sess-f") }},
		{"SessionDeleteByUser", func() error { return tx.DeleteSessionsByUser(ctx, "user-b") }},
		{"SessionCreate", func() error { return tx.CreateSession(ctx, session(t, "sess-c", "user-b", "cred-b", 2)) }},
		{"SpecUpsert", func() error { return tx.UpsertSpec(ctx, spec(t, "spec-a", 1)) }},
		{"SpecDelete", func() error { return tx.DeleteSpec(ctx, "spec-f") }},
		{"RolloutUpsert", func() error { return tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-a", 1)) }},
		{"RolloutDeleteBySpec", func() error { return tx.DeleteRolloutsBySpec(ctx, "spec-b") }},
		{"RolloutUpsert", func() error { return tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-c", 2)) }},
		{"RolloutDelete", func() error { return tx.DeleteRollout(ctx, model.RolloutID("spec-d", "agent-b")) }},
		{"ExecutionUpsert", func() error { return tx.UpsertExecution(ctx, execution(t, "a", 1)) }},
		{"", func() error { return tx.UpsertExecution(ctx, rewritten) }},
		{"ExecutionDelete", func() error { return tx.DeleteExecution(ctx, execution(t, "d", 1).ID()) }},
	}
	var want []string
	for i, w := range writes {
		if err := w.write(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if w.op != "" {
			want = append(want, w.op)
		}
	}
	var got []string
	for _, op := range tx.Ops() {
		got = append(got, strings.TrimPrefix(fmt.Sprintf("%T", op.GetOp()), "*raftv1.Op_"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("recorded operations %v, want %v", got, want)
	}

	read := everything(t, tx)
	if err := base.WithTx(ctx, func(dst storage.Storage) error { return overlay.ApplyOps(ctx, dst, tx.Ops()) }); err != nil {
		t.Fatal(err)
	}
	committed := everything(t, base)
	for key, row := range read {
		switch held, ok := committed[key]; {
		case !ok:
			t.Errorf("%s: the transaction read a row the commit does not hold", key)
		case held != row:
			t.Errorf("%s: the commit holds another row than the transaction read", key)
		}
	}
	for key := range committed {
		if _, ok := read[key]; !ok {
			t.Errorf("%s: the commit holds a row the transaction did not read", key)
		}
	}
}

func ignoreNotFound(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

// everything reads every row of the contract's kinds from s, in its wire form, by
// kind and ID: by scan or listing where the kind has one, and by the IDs and users
// the contract uses where it has none.
func everything(t *testing.T, s storage.Storage) map[string]string {
	t.Helper()
	ctx := t.Context()
	out := map[string]string{}
	add := func(kind, id string, m proto.Message) {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out[kind+"/"+id] = string(b)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	agents, err := storage.AllAgents(ctx, s, nil)
	must(err)
	for _, a := range agents {
		add("agent", a.ID(), wire.AgentToProto(a))
	}
	users, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.UserListResult, error) { return s.ListUsers(ctx, nil, o) })
	must(err)
	for _, u := range users {
		add("user", u.ID(), wire.UserToProto(u))
	}
	roles, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.RoleListResult, error) { return s.ListRoles(ctx, nil, o) })
	must(err)
	for _, r := range roles {
		add("role", r.ID(), wire.RoleToProto(r))
	}
	specs, err := storage.AllSpecs(ctx, s, nil)
	must(err)
	for _, ts := range specs {
		m, err := wire.SpecToProto(ts)
		must(err)
		add("spec", ts.ID(), m)
	}
	rollouts, err := storage.AllRollouts(ctx, s, nil)
	must(err)
	for _, r := range rollouts {
		add("rollout", r.ID(), wire.RolloutToProto(r))
	}
	runs, err := storage.AllExecutions(ctx, s, nil)
	must(err)
	for _, e := range runs {
		add("execution", e.ID(), wire.ExecutionToProto(e))
	}
	for _, l := range "abcdefg" {
		if c, err := s.GetAgentCredential(ctx, "agent-"+string(l)); err == nil {
			add("agent credential", c.ID(), wire.AgentCredentialToProto(c))
		} else if !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(err)
		}
		if v, err := s.GetVerifier(ctx, "ver-"+string(l)); err == nil {
			add("verifier", v.ID(), wire.VerifierToProto(v))
		} else if !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(err)
		}
		creds, err := s.ListCredentialsByUser(ctx, "user-"+string(l))
		must(err)
		for _, c := range creds {
			add("credential", c.ID(), wire.CredentialToProto(c))
		}
		sessions, err := s.ListSessionsByUser(ctx, "user-"+string(l))
		must(err)
		for _, ss := range sessions {
			add("session", ss.ID(), wire.SessionToProto(ss))
		}
	}
	return out
}

// TestOverlayScanFailsAsListingDoes checks the errors of the transaction's scans
// and listings: a filter of another backend or a nil pointer to the filter type, a
// cursor of another backend and a done context.
func TestOverlayScanFailsAsListingDoes(t *testing.T) {
	ctx := t.Context()
	tx := overlay.New(seeded(t))
	if err := tx.UpsertAgent(ctx, agent(t, "agent-a", 1, "prod")); err != nil {
		t.Fatal(err)
	}
	scans := []struct {
		name      string
		nilFilter any
		scan      func(context.Context, any) error
		list      func(context.Context, any, storage.ListOptions) error
	}{
		{
			"agents", (*inmemory.AgentFilter)(nil),
			func(ctx context.Context, f any) error { _, err := tx.ScanAgents(ctx, f); return err },
			func(ctx context.Context, f any, o storage.ListOptions) error {
				_, err := tx.ListAgents(ctx, f, o)
				return err
			},
		},
		{
			"rollouts", (*inmemory.RolloutFilter)(nil),
			func(ctx context.Context, f any) error { _, err := tx.ScanRollouts(ctx, f); return err },
			func(ctx context.Context, f any, o storage.ListOptions) error {
				_, err := tx.ListRollouts(ctx, f, o)
				return err
			},
		},
		{
			"specs", (*inmemory.SpecFilter)(nil),
			func(ctx context.Context, f any) error { _, err := tx.ScanSpecs(ctx, f); return err },
			func(ctx context.Context, f any, o storage.ListOptions) error {
				_, err := tx.ListSpecs(ctx, f, o)
				return err
			},
		},
		{
			"executions", (*inmemory.ExecutionFilter)(nil),
			func(ctx context.Context, f any) error { _, err := tx.ScanExecutions(ctx, f); return err },
			func(ctx context.Context, f any, o storage.ListOptions) error {
				_, err := tx.ListExecutions(ctx, f, o)
				return err
			},
		},
	}
	foreign, err := seeded(t).ListAgents(ctx, nil, storage.ListOptions{Limit: 1})
	if err != nil || foreign.NextCursor == "" {
		t.Fatalf("the in-memory listing gave no cursor to try: %v", err)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	for _, sc := range scans {
		for _, f := range []struct {
			name   string
			filter any
		}{{"foreign", struct{}{}}, {"nil pointer", sc.nilFilter}} {
			if err := sc.scan(ctx, f.filter); !errors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("scan of %s with a %s filter: %v, want ErrInvalidArgument", sc.name, f.name, err)
			}
			if err := sc.list(ctx, f.filter, storage.ListOptions{}); !errors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("listing of %s with a %s filter: %v, want ErrInvalidArgument", sc.name, f.name, err)
			}
		}
		for _, cursor := range []string{"not a cursor", foreign.NextCursor} {
			if err := sc.list(ctx, nil, storage.ListOptions{Cursor: cursor}); !errors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("listing of %s with cursor %q: %v, want ErrInvalidArgument", sc.name, cursor, err)
			}
		}
		if err := sc.scan(done, nil); !errors.Is(err, context.Canceled) {
			t.Errorf("scan of %s with a done context: %v, want context.Canceled", sc.name, err)
		}
		if err := sc.list(done, nil, storage.ListOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("listing of %s with a done context: %v, want context.Canceled", sc.name, err)
		}
	}
}
