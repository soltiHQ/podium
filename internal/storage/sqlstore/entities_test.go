package sqlstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// These tests check what the contract suite cannot see of the entity stores: the
// routing of their reads, the snapshot of a scan, the paging of a listing the Go
// filter thins out, a bound outside the range of a time column, and the cap on
// an index key. The suite itself runs in contract_test.go.

// testTime is a creation time in a zone other than UTC, between whole
// microseconds.
var testTime = time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("test", 3*60*60))

func testAgent(t *testing.T, id string, created time.Time) *model.Agent {
	t.Helper()
	a, err := model.NewAgent(id, "edge "+id, "https://"+id+".example.net")
	if err != nil {
		t.Fatal(err)
	}
	a.SetCreatedAt(created)
	a.SetUpdatedAt(created)
	a.SetLastSeenAt(created)
	return a
}

func testAgentCredential(t *testing.T, agentID string, created time.Time) *model.AgentCredential {
	t.Helper()
	c, err := model.NewStoredAgentCredential(agentID, "token-"+agentID)
	if err != nil {
		t.Fatal(err)
	}
	c.SetCreatedAt(created)
	return c
}

func testSpec(t *testing.T, id string, created time.Time) *model.Spec {
	t.Helper()
	s, err := model.NewSpec(id, "job "+id, "slot")
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreatedAt(created)
	return s
}

func testRollout(t *testing.T, specID, agentID string, created time.Time) *model.Rollout {
	t.Helper()
	r, err := model.NewRollout(specID, agentID, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.SetCreatedAt(created)
	return r
}

func testExecution(t *testing.T, agentID string, captured time.Time) *model.ExecutionRecord {
	t.Helper()
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
		AgentID: agentID, TaskName: "build", TaskUID: "uid", Generation: 1, Attempt: 1, Status: "succeeded", CapturedAt: captured,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// seed writes one row of each entity group of a store, keyed by tag, and
// returns the IDs it wrote by the read that must find them: the reads of a store
// are told apart by which database's rows they return.
func seed(t *testing.T, s *Store, tag string) map[string]string {
	t.Helper()
	var (
		agent     = testAgent(t, "agent-"+tag, testTime)
		agentCred = testAgentCredential(t, agent.ID(), testTime)
		spec      = testSpec(t, "spec-"+tag, testTime)
		rollout   = testRollout(t, spec.ID(), agent.ID(), testTime)
		execution = testExecution(t, agent.ID(), testTime)
	)
	ctx := t.Context()
	err := errors.Join(
		s.UpsertAgent(ctx, agent), s.UpsertAgentCredential(ctx, agentCred), s.UpsertSpec(ctx, spec),
		s.UpsertRollout(ctx, rollout), s.UpsertExecution(ctx, execution),
	)
	if err != nil {
		t.Fatalf("seed %s: %v", tag, err)
	}
	return map[string]string{
		"ListAgents": agent.ID(), "ScanAgents": agent.ID(),
		"ListAgentCredentials": agentCred.ID(),
		"ListSpecs":            spec.ID(), "ScanSpecs": spec.ID(),
		"ListRollouts": rollout.ID(), "ScanRollouts": rollout.ID(),
		"ListExecutions": execution.ID(), "ScanExecutions": execution.ID(),
		"CountAgents": "1", "CountRollouts": "1",
	}
}

// entityStore is what a store, and the handle of a transaction, keep of the
// entities these tests write.
type entityStore interface {
	storage.AgentStore
	storage.AgentCredentialStore
	storage.SpecStore
	storage.RolloutStore
	storage.ExecutionStore
}

// read returns what each listing, scan and count of s finds, by read, as the
// IDs of the rows joined, or the count.
func read(ctx context.Context, s entityStore) (map[string]string, error) {
	var (
		got  = map[string]string{}
		errs []error
		opts = storage.ListOptions{}
	)
	found := func(name string, ids []string, err error) {
		got[name] = strings.Join(ids, ",")
		errs = append(errs, err)
	}
	agents, err := s.ListAgents(ctx, nil, opts)
	found("ListAgents", pageIDs(agents), err)
	credentials, err := s.ListAgentCredentials(ctx, opts)
	found("ListAgentCredentials", pageIDs(credentials), err)
	specs, err := s.ListSpecs(ctx, nil, opts)
	found("ListSpecs", pageIDs(specs), err)
	rollouts, err := s.ListRollouts(ctx, nil, opts)
	found("ListRollouts", pageIDs(rollouts), err)
	executions, err := s.ListExecutions(ctx, nil, opts)
	found("ListExecutions", pageIDs(executions), err)
	if sc, ok := s.(storage.Scanner); ok {
		scannedAgents, err := sc.ScanAgents(ctx, nil)
		found("ScanAgents", idsOf(scannedAgents), err)
		scannedSpecs, err := sc.ScanSpecs(ctx, nil)
		found("ScanSpecs", idsOf(scannedSpecs), err)
		scannedRollouts, err := sc.ScanRollouts(ctx, nil)
		found("ScanRollouts", idsOf(scannedRollouts), err)
		scannedExecutions, err := sc.ScanExecutions(ctx, nil)
		found("ScanExecutions", idsOf(scannedExecutions), err)
	}
	if c, ok := s.(storage.Counter); ok {
		agentCounts, err := c.CountAgents(ctx)
		found("CountAgents", []string{fmt.Sprint(len(agentCounts))}, err)
		rolloutCounts, err := c.CountRollouts(ctx)
		found("CountRollouts", []string{fmt.Sprint(len(rolloutCounts))}, err)
	}
	return got, errors.Join(errs...)
}

// pageIDs returns the IDs of a page's rows, none for no page.
func pageIDs[T interface{ ID() string }](page *storage.ListResult[T]) []string {
	if page == nil {
		return nil
	}
	return idsOf(page.Items)
}

// idsOf returns the IDs of rows, in order.
func idsOf[T interface{ ID() string }](rows []T) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID()
	}
	return ids
}

// TestReadsFollowTheRouting gives the store a replica that holds other rows than
// the primary: every listing, scan and count of a context marked read-only reads
// the replica, and of an unmarked one the primary; so does every read inside a
// transaction, its context marked or not.
func TestReadsFollowTheRouting(t *testing.T) {
	primary, replica := migrated(t), migrated(t)
	withReplica := openStore(t, Options{Primary: primary, Replica: replica})
	onPrimary := seed(t, withReplica, "primary")
	onReplica := seed(t, openStore(t, Options{Primary: replica}), "replica")
	marked := storage.MarkReadOnly(t.Context())

	check := func(t *testing.T, ctx context.Context, what string, s entityStore, want map[string]string) {
		t.Helper()
		got, err := read(ctx, s)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d reads, want %d", what, len(got), len(want))
		}
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s: %s = %q, want %q", what, name, got[name], w)
			}
		}
	}
	check(t, marked, "a marked context", withReplica, onReplica)
	check(t, t.Context(), "an unmarked context", withReplica, onPrimary)
	check(t, marked, "a marked context, no replica", openStore(t, Options{Primary: primary}), onPrimary)
	err := withReplica.transact(marked, func(h *handle) error {
		check(t, marked, "inside a transaction", h, onPrimary)
		return nil
	})
	if err != nil {
		t.Fatalf("transact: %v", err)
	}
	// A point lookup reads the primary, whatever the context says.
	if _, err := withReplica.GetAgent(marked, "agent-primary"); err != nil {
		t.Errorf("GetAgent of a row on the primary, marked: %v", err)
	}
	if _, err := withReplica.GetAgent(marked, "agent-replica"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetAgent of a row on the replica, marked: %v, want storage.ErrNotFound", err)
	}
}

// TestScanTakesASnapshot checks that a scan outside a transaction reads in one
// read-only REPEATABLE READ transaction, and one inside a transaction reads that
// transaction.
func TestScanTakesASnapshot(t *testing.T) {
	s, ctx := openStore(t, Options{Primary: migrated(t)}), t.Context()
	mode := func(r *reader) (isolation, readOnly string, err error) {
		err = r.q.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).
			Scan(&isolation, &readOnly)
		return
	}
	err := s.snapshot(ctx, func(r *reader) error {
		isolation, readOnly, err := mode(r)
		if err != nil || isolation != "repeatable read" || readOnly != "on" {
			return fmt.Errorf("a scan reads in %q, read-only %q, %v; want repeatable read, read-only", isolation, readOnly, err)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
	err = s.transact(ctx, func(h *handle) error {
		return h.snapshot(ctx, func(r *reader) error {
			isolation, readOnly, err := mode(r)
			if err != nil || isolation != "read committed" || readOnly != "off" {
				return fmt.Errorf("a scan in a transaction reads in %q, read-only %q, %v; want the write transaction", isolation, readOnly, err)
			}
			return nil
		})
	})
	if err != nil {
		t.Error(err)
	}
}

// TestFilteredListingPages lists 150 agents of which the Query matches every
// seventh, across ties, at several page sizes: the Go filter thins each keyset
// batch out, and every page but the last is full, no match is skipped or
// returned twice, and the cursor names the last row returned.
func TestFilteredListingPages(t *testing.T) {
	s, ctx := openStore(t, Options{Primary: migrated(t)}), t.Context()
	var want []*model.Agent
	err := s.transact(ctx, func(tx *handle) error {
		for i := range 150 {
			// Five agents to a minute, so that the rows tie on creation time.
			name := fmt.Sprintf("plain-%03d", i)
			if i%7 == 0 {
				name = fmt.Sprintf("Match-%03d", i)
			}
			a := testAgent(t, name, testTime.Add(time.Duration(i/5)*time.Minute))
			if err := tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
			if i%7 == 0 {
				want = append(want, a)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(want, func(a, b *model.Agent) int {
		if c := b.CreatedAt().Compare(a.CreatedAt()); c != 0 {
			return c
		}
		return strings.Compare(a.ID(), b.ID())
	})
	filter := s.BuildAgentFilter(storage.AgentQueryCriteria{Query: "  MATCH-"})

	for _, limit := range []int{1, 3, 5, 21, len(want), len(want) + 1, 500} {
		var got []string
		cursor := ""
		for range 1000 {
			page, err := s.ListAgents(ctx, filter, storage.ListOptions{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			got = append(got, idsOf(page.Items)...)
			if page.NextCursor == "" {
				break
			}
			if len(page.Items) != limit {
				t.Fatalf("limit %d: a page of %d rows before the last", limit, len(page.Items))
			}
			cursor = page.NextCursor
		}
		if !slices.Equal(got, idsOf(want)) {
			t.Errorf("limit %d: %d rows, want %d in order", limit, len(got), len(want))
		}
	}
}

// TestStaleAtBeforeOutsideTheRange checks a staleness bound a time column cannot
// hold: below the years 1678 to 2262 it matches the agents with no deadline
// only, above it every agent, as memory answers, since every deadline stored
// lies inside the range.
func TestStaleAtBeforeOutsideTheRange(t *testing.T) {
	s, ctx := openStore(t, Options{Primary: migrated(t)}), t.Context()
	for id, stale := range map[string]time.Time{
		"never": {}, "past": testTime.Add(-time.Hour), "future": testTime.Add(time.Hour),
	} {
		a := testAgent(t, id, testTime)
		a.SetStaleAt(stale)
		if err := s.UpsertAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		bound time.Time
		want  []string
	}{
		{"no bound", time.Time{}, []string{"future", "never", "past"}},
		{"between them", testTime, []string{"never", "past"}},
		{"before 1678", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), []string{"never"}},
		{"after 2262", time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), []string{"future", "never", "past"}},
		{"the Unix epoch", time.Unix(0, 0), []string{"never"}},
	} {
		page, err := s.ListAgents(ctx, s.BuildAgentFilter(storage.AgentQueryCriteria{StaleAtBefore: tc.bound}), storage.ListOptions{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := idsOf(page.Items); !slices.Equal(got, tc.want) {
			t.Errorf("%s: agents %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEntityKeys checks that an ID with a NUL, which bytea carries, is stored and
// found, and that an ID of exactly the cap is stored while one byte more is
// refused.
func TestEntityKeys(t *testing.T) {
	s, ctx := openStore(t, Options{Primary: migrated(t)}), t.Context()
	nul := testAgent(t, "agent\x00nul", testTime)
	if err := s.UpsertAgent(ctx, nul); err != nil {
		t.Fatalf("UpsertAgent of an ID with a NUL: %v", err)
	}
	if got, err := s.GetAgent(ctx, nul.ID()); err != nil || got.ID() != nul.ID() {
		t.Errorf("GetAgent of an ID with a NUL: %v, %v", got, err)
	}

	random := make([]byte, maxKeyBytes/2)
	_, _ = rand.Read(random)
	atCap := hex.EncodeToString(random)
	if len(atCap) != maxKeyBytes {
		t.Fatalf("test ID is %d bytes, want %d", len(atCap), maxKeyBytes)
	}
	if err := s.UpsertAgent(ctx, testAgent(t, atCap, testTime)); err != nil {
		t.Errorf("UpsertAgent of an ID of %d bytes: %v", maxKeyBytes, err)
	}
	if _, err := s.GetAgent(ctx, atCap); err != nil {
		t.Errorf("GetAgent of an ID of %d bytes: %v", maxKeyBytes, err)
	}
}

// TestIndexKeyIsRefusedBeforeAnyStatement writes, in one transaction, a row with
// a key over the cap for each entity of the store: each write is refused with
// storage.ErrInvalidArgument, none leaves the transaction unusable, and the
// transaction commits what it wrote after them. A write outside a transaction is
// refused alike.
func TestIndexKeyIsRefusedBeforeAnyStatement(t *testing.T) {
	s, ctx := openStore(t, Options{Primary: migrated(t)}), t.Context()
	long := strings.Repeat("k", maxKeyBytes+1)
	short := "k"
	for _, tc := range []struct {
		name  string
		write func(tx *handle) error
	}{
		{"an agent ID", func(tx *handle) error { return tx.UpsertAgent(ctx, testAgent(t, long, testTime)) }},
		{"an agent credential's agent ID", func(tx *handle) error {
			return tx.UpsertAgentCredential(ctx, testAgentCredential(t, long, testTime))
		}},
		{"a spec ID", func(tx *handle) error { return tx.UpsertSpec(ctx, testSpec(t, long, testTime)) }},
		{"a rollout's spec ID", func(tx *handle) error {
			return tx.UpsertRollout(ctx, testRollout(t, long, short, testTime))
		}},
		{"a rollout's agent ID", func(tx *handle) error {
			return tx.UpsertRollout(ctx, testRollout(t, short, long, testTime))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.transact(ctx, func(tx *handle) error {
				if err := tc.write(tx); !errors.Is(err, storage.ErrInvalidArgument) {
					t.Errorf("the write: %v, want storage.ErrInvalidArgument", err)
				}
				return tx.UpsertAgent(ctx, testAgent(t, "after "+tc.name, testTime))
			})
			if err != nil {
				t.Errorf("the transaction after the refused write: %v", err)
			}
		})
	}
	if err := s.UpsertAgent(ctx, testAgent(t, long, testTime)); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("the write outside a transaction: %v, want storage.ErrInvalidArgument", err)
	}
}
