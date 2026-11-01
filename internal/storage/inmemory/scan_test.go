package inmemory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// fillTied stores n entities that are not inserted in ID order and share creation
// times: those repeat every 97 seconds, so the ID decides the order among about
// n/97 entities at a time. n must not be a multiple of 7.
func fillTied(t *testing.T, s *GenericStore[*testEntity], n int) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := range n {
		k := i * 7 % n
		e := newTestEntity(fmt.Sprintf("e-%04d", k), base.Add(time.Duration(k%97)*time.Second))
		e.payload["n"] = strconv.Itoa(k)
		requireNoErr(t, s.Upsert(context.Background(), e))
	}
}

// pagedAll drains List the way storage.AllAgents and its siblings do for a store
// that is not a storage.Scanner.
func pagedAll(t *testing.T, s *GenericStore[*testEntity], pred func(*testEntity) bool) []*testEntity {
	t.Helper()
	ctx := context.Background()
	rows, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.ListResult[*testEntity], error) {
		return s.List(ctx, pred, o)
	})
	requireNoErr(t, err)
	return rows
}

func firstDifference[T any](a, b []T) int {
	n := min(len(a), len(b))
	for i := range n {
		if !reflect.DeepEqual(a[i], b[i]) {
			return i
		}
	}
	return n
}

func TestAllMatchesCollectPages(t *testing.T) {
	t.Parallel()

	const rows = 1237 // three pages of storage.MaxListLimit, the last one short
	s := NewGenericStore[*testEntity]()
	fillTied(t, s, rows)

	for _, tc := range []struct {
		name string
		pred func(*testEntity) bool
		want int
	}{
		{"every entity", nil, rows},
		{"a third of them", func(e *testEntity) bool { n, _ := strconv.Atoi(e.payload["n"]); return n%3 == 0 }, 413},
		{"none", func(*testEntity) bool { return false }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := pagedAll(t, s, tc.pred)
			got := s.All(tc.pred, nil)
			if len(got) != tc.want {
				t.Fatalf("All returned %d entities, want %d", len(got), tc.want)
			}
			// DeepEqual tells a nil slice from an empty one: both must agree on "nothing matched".
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("All differs from CollectPages (%d and %d entities), first at %d", len(got), len(want), firstDifference(got, want))
			}

			for i, e := range got {
				s.mu.RLock()
				stored := s.data[e.ID()]
				s.mu.RUnlock()
				if e == stored {
					t.Fatalf("entity %d (%s) is the stored value, not a clone", i, e.ID())
				}
				e.payload["k"] = "mutated"
			}
			for _, e := range s.All(tc.pred, nil) {
				if e.payload["k"] != "v" {
					t.Fatalf("a change to a returned entity reached %s in the store", e.ID())
				}
			}
		})
	}
}

func TestAllIsSinglePass(t *testing.T) {
	t.Parallel()

	for _, rows := range []int{1237, 5000} {
		t.Run(strconv.Itoa(rows), func(t *testing.T) {
			s := NewGenericStore[*testEntity]()
			fillTied(t, s, rows)

			calls := 0
			got := s.All(func(*testEntity) bool { calls++; return true }, nil)
			if len(got) != rows || calls != rows {
				t.Fatalf("All of %d entities returned %d and ran the predicate %d times, want once each", rows, len(got), calls)
			}
		})
	}
}

func TestAllOrdersByLessWhenGiven(t *testing.T) {
	t.Parallel()

	s := NewGenericStore[*testEntity]()
	fillTied(t, s, 100)

	byIDDesc := func(a, b *testEntity) bool { return a.ID() > b.ID() }
	got := s.All(nil, byIDDesc)
	if len(got) != 100 || !sort.SliceIsSorted(got, func(i, j int) bool { return byIDDesc(got[i], got[j]) }) {
		t.Fatalf("All did not order its %d entities by the given less", len(got))
	}
	if listed := s.All(nil, nil); got[0].ID() == listed[0].ID() {
		t.Fatalf("the order given and the listing order start with %s alike, the test proves nothing", got[0].ID())
	}
}

func TestAllOfNothingIsNil(t *testing.T) {
	t.Parallel()

	if got := NewGenericStore[*testEntity]().All(nil, nil); got != nil {
		t.Fatalf("All of an empty store = %#v, want nil as CollectPages returns", got)
	}
}

// It is a data-race test: the order and the absence of repeats hold whatever the
// locking, so only the race detector, which CI runs it under, can fail it for a
// missing lock.
func TestAllIsOrderedWithoutRepeatsUnderConcurrentWritersRace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()
	fillTied(t, s, 300)

	stop := make(chan struct{})
	done := make(chan struct{})
	for w := range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			base := time.Unix(1_700_000_000, 0).UTC()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := (i*13 + w) % 400 // beyond 300 it adds, below it replaces or removes
				id := fmt.Sprintf("e-%04d", k)
				if i%3 == 0 {
					_ = s.Delete(ctx, id)
				} else {
					_ = s.Upsert(ctx, newTestEntity(id, base.Add(time.Duration(k%97)*time.Second)))
				}
			}
		}()
	}
	defer func() {
		close(stop)
		for range 4 {
			<-done
		}
	}()

	for range 200 {
		got := s.All(nil, nil)
		for i := 1; i < len(got); i++ {
			if !listLess(got[i-1], got[i]) {
				t.Fatalf("All returned %s before %s: not one ordered state without repeats", got[i-1].ID(), got[i].ID())
			}
		}
	}
}

func TestAllStopsOnceContextIsDone(t *testing.T) {
	t.Parallel()

	const rows = 5000
	s := NewGenericStore[*testEntity]()
	fillTied(t, s, rows)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	got, err := s.all(ctx, func(*testEntity) bool {
		if calls++; calls == 1500 {
			cancel()
		}
		return true
	}, nil)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("all of a canceled context = %d entities, %v, want nothing and context.Canceled", len(got), err)
	}
	if calls >= rows {
		t.Fatalf("all ran the predicate on all %d entities after its context was canceled", rows)
	}
}

// namedScan is one Scanner method, and the listing it replaces, called with a
// filter of any kind. nilFilter is a nil pointer to the filter type of the store.
type namedScan struct {
	name      string
	nilFilter any
	call      func(context.Context, storage.Scanner, any) error
	list      func(context.Context, storage.Storage, any) error
}

var scanCalls = []namedScan{
	{
		"agents", (*AgentFilter)(nil),
		func(ctx context.Context, s storage.Scanner, f any) error {
			_, err := s.ScanAgents(ctx, f)
			return err
		},
		func(ctx context.Context, s storage.Storage, f any) error {
			_, err := s.ListAgents(ctx, f, storage.ListOptions{})
			return err
		},
	},
	{
		"rollouts", (*RolloutFilter)(nil),
		func(ctx context.Context, s storage.Scanner, f any) error {
			_, err := s.ScanRollouts(ctx, f)
			return err
		},
		func(ctx context.Context, s storage.Storage, f any) error {
			_, err := s.ListRollouts(ctx, f, storage.ListOptions{})
			return err
		},
	},
	{
		"specs", (*SpecFilter)(nil),
		func(ctx context.Context, s storage.Scanner, f any) error {
			_, err := s.ScanSpecs(ctx, f)
			return err
		},
		func(ctx context.Context, s storage.Storage, f any) error {
			_, err := s.ListSpecs(ctx, f, storage.ListOptions{})
			return err
		},
	},
	{
		"executions", (*ExecutionFilter)(nil),
		func(ctx context.Context, s storage.Scanner, f any) error {
			_, err := s.ScanExecutions(ctx, f)
			return err
		},
		func(ctx context.Context, s storage.Storage, f any) error {
			_, err := s.ListExecutions(ctx, f, storage.ListOptions{})
			return err
		},
	},
}

// checkScan compares a scan with the listing drained page by page, for a filter
// that matches every row, one that matches some and one that matches none: the
// same rows in the same order. A second scan must hand out other clones than
// the first, so that no scan returns the rows the store holds.
func checkScan[T, F any](t *testing.T, kind string, total int, scanFn func(context.Context, F) ([]T, error), listFn func(context.Context, F, storage.ListOptions) (*storage.ListResult[T], error), some, none F) {
	t.Helper()
	ctx := context.Background()
	var every F
	for _, tc := range []struct {
		name   string
		filter F
		count  func(n int) bool
	}{
		{"every row", every, func(n int) bool { return n == total }},
		{"some rows", some, func(n int) bool { return n > 0 && n < total }},
		{"no row", none, func(n int) bool { return n == 0 }},
	} {
		t.Run(kind+"/"+tc.name, func(t *testing.T) {
			want, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.ListResult[T], error) {
				return listFn(ctx, tc.filter, o)
			})
			requireNoErr(t, err)
			got, err := scanFn(ctx, tc.filter)
			requireNoErr(t, err)
			if !tc.count(len(got)) {
				t.Fatalf("scan returned %d of %d rows", len(got), total)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scan differs from CollectPages (%d and %d rows), first at %d", len(got), len(want), firstDifference(got, want))
			}
			again, err := scanFn(ctx, tc.filter)
			requireNoErr(t, err)
			for i := range got {
				if any(got[i]) == any(again[i]) {
					t.Fatalf("scans share the row at %d: it is not a clone", i)
				}
			}
		})
	}
}

func TestScannerMatchesCollectPages(t *testing.T) {
	ctx := context.Background()
	s := New()
	// All 3 711 rollouts and all specs share one creation time, so their order is the
	// ID tie-break alone. The executions have distinct creation times that no ID order
	// follows, so they pin the CreatedAt key. The agents do not: the fixture creates
	// them newest first with ascending IDs, so CreatedAt DESC and ID ASC give the
	// same order for them.
	const rows = 1237
	requireNoErr(t, storagetest.Seed(ctx, s, storagetest.Fixture{Agents: rows, HistoryRows: rows}))

	check := func(t *testing.T, st storage.Storage) {
		t.Helper()
		sc, ok := st.(storage.Scanner)
		if !ok {
			t.Fatalf("%T is not a storage.Scanner", st)
		}
		checkScan[*model.Agent, storage.AgentFilter](t, "agents", rows, sc.ScanAgents, st.ListAgents,
			NewAgentFilter().ByLabel("region", "eu-west"),
			NewAgentFilter().ByLabel("region", "mars"))
		checkScan[*model.Rollout, storage.RolloutFilter](t, "rollouts", rows*storagetest.RolloutsPerAgent, sc.ScanRollouts, st.ListRollouts,
			st.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: storagetest.SpecID(0)}),
			st.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: "no-such-spec"}))
		checkScan[*model.Spec, storage.SpecFilter](t, "specs", storagetest.SpecCount, sc.ScanSpecs, st.ListSpecs,
			st.BuildSpecFilter(storage.SpecQueryCriteria{Query: "job-1"}),
			st.BuildSpecFilter(storage.SpecQueryCriteria{Query: "no-such-job"}))
		checkScan[*model.ExecutionRecord, storage.ExecutionFilter](t, "executions", rows, sc.ScanExecutions, st.ListExecutions,
			st.BuildExecutionFilter(storage.ExecutionQueryCriteria{Statuses: []string{"failed"}}),
			st.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "no-such-agent"}))
	}

	t.Run("store", func(t *testing.T) { check(t, s) })
	t.Run("transaction", func(t *testing.T) {
		requireNoErr(t, s.WithTx(ctx, func(tx storage.Storage) error { check(t, tx); return nil }))
	})
}

func TestScanFailsAsListingDoes(t *testing.T) {
	t.Parallel()

	s := New()
	requireNoErr(t, s.UpsertAgent(context.Background(), mkAgent(t, "a1")))

	t.Run("a filter of another backend", func(t *testing.T) {
		for _, tc := range scanCalls {
			// A nil pointer to the filter type is no filter either: it would panic when asked to match.
			for _, f := range []struct {
				name   string
				filter any
			}{{"foreign", struct{}{}}, {"nil pointer", tc.nilFilter}} {
				if err := tc.call(context.Background(), s, f.filter); !errors.Is(err, storage.ErrInvalidArgument) {
					t.Errorf("Scan%s with a %s filter: err = %v, want ErrInvalidArgument", tc.name, f.name, err)
				}
				if err := tc.list(context.Background(), s, f.filter); !errors.Is(err, storage.ErrInvalidArgument) {
					t.Errorf("List%s with a %s filter: err = %v, want ErrInvalidArgument", tc.name, f.name, err)
				}
			}
		}
	})

	t.Run("a canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Whether the store holds rows or not, as CollectPages fails before its first page.
		for _, st := range []*Store{s, New()} {
			for _, tc := range scanCalls {
				if err := tc.call(ctx, st, nil); !errors.Is(err, context.Canceled) {
					t.Errorf("%s: err = %v, want context.Canceled", tc.name, err)
				}
			}
		}
	})

	t.Run("an empty store", func(t *testing.T) {
		rows, err := New().ScanAgents(context.Background(), nil)
		if rows != nil || err != nil {
			t.Fatalf("scan of an empty store = %#v, %v, want nil, nil as CollectPages returns", rows, err)
		}
	})
}

func TestScanRunsPredicateOncePerRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()
	const rows = 1237
	requireNoErr(t, storagetest.Seed(ctx, s, storagetest.Fixture{Agents: rows}))

	calls := 0
	counted := &AgentFilter{predicates: []func(*model.Agent) bool{func(*model.Agent) bool { calls++; return true }}}
	got, err := s.ScanAgents(ctx, counted)
	requireNoErr(t, err)
	if len(got) != rows || calls != rows {
		t.Fatalf("a scan of %d agents returned %d and ran the filter %d times, want once each", rows, len(got), calls)
	}
}

func TestScanWaitsForTransaction(t *testing.T) {
	ctx := context.Background()
	agent := mkAgent(t, "a1")
	rollout, err := model.NewRollout("spec-1", "agent-1", 1)
	requireNoErr(t, err)
	spec, err := model.NewSpec("spec-1", "job", "slot")
	requireNoErr(t, err)
	execution := archiveRow(t, 1)

	for _, tc := range []struct {
		name  string
		write func(storage.Storage) error
		count func(*Store) (int, error)
	}{
		{
			"agents",
			func(tx storage.Storage) error { return tx.UpsertAgent(ctx, agent) },
			func(s *Store) (int, error) { rows, err := s.ScanAgents(ctx, nil); return len(rows), err },
		},
		{
			"rollouts",
			func(tx storage.Storage) error { return tx.UpsertRollout(ctx, rollout) },
			func(s *Store) (int, error) { rows, err := s.ScanRollouts(ctx, nil); return len(rows), err },
		},
		{
			"specs",
			func(tx storage.Storage) error { return tx.UpsertSpec(ctx, spec) },
			func(s *Store) (int, error) { rows, err := s.ScanSpecs(ctx, nil); return len(rows), err },
		},
		{
			"executions",
			func(tx storage.Storage) error { return tx.UpsertExecution(ctx, execution) },
			func(s *Store) (int, error) { rows, err := s.ScanExecutions(ctx, nil); return len(rows), err },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			entered, release := make(chan struct{}), make(chan struct{})
			txDone := make(chan error, 1)
			go func() {
				txDone <- s.WithTx(ctx, func(tx storage.Storage) error {
					if err := tc.write(tx); err != nil {
						return err
					}
					close(entered)
					<-release
					return errors.New("abort")
				})
			}()
			<-entered

			type result struct {
				rows int
				err  error
			}
			scanned := make(chan result, 1)
			go func() { rows, err := tc.count(s); scanned <- result{rows, err} }()
			select {
			case r := <-scanned:
				close(release)
				txErr := <-txDone
				t.Fatalf("a scan ran inside the transaction: %d rows, %v (the transaction ended with %v)", r.rows, r.err, txErr)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-txDone; err == nil {
				t.Fatal("aborted transaction reported success")
			}
			if r := <-scanned; r.err != nil || r.rows != 0 {
				t.Fatalf("scan after the rollback = %d rows, %v, want none", r.rows, r.err)
			}
		})
	}
}

func TestScanInsideTransactionSeesItsOwnWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()
	requireNoErr(t, s.UpsertAgent(ctx, mkAgent(t, "kept")))

	errAbort := errors.New("abort")
	err := s.WithTx(ctx, func(tx storage.Storage) error {
		requireNoErr(t, tx.UpsertAgent(ctx, mkAgent(t, "added")))
		requireNoErr(t, tx.DeleteAgent(ctx, "kept"))
		rows, err := storage.AllAgents(ctx, tx, nil)
		requireNoErr(t, err)
		if len(rows) != 1 || rows[0].ID() != "added" {
			t.Fatalf("a scan in the transaction returned %d agents, want only the one it added", len(rows))
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("WithTx = %v, want the callback's error", err)
	}
	rows, err := storage.AllAgents(ctx, s, nil)
	requireNoErr(t, err)
	if len(rows) != 1 || rows[0].ID() != "kept" {
		t.Fatalf("after the rollback a scan returned %d agents, want only the one it kept", len(rows))
	}
}
