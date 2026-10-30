package durable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// While a transaction runs, reads do not queue behind it, and queued transactions
// return as soon as their context ends, without aborting it.
func TestQueuedWritesCancelAndReadsDoNotQueueBehindActiveTransaction(t *testing.T) {
	s, _ := newTestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	writer := make(chan error, 1)
	go func() {
		writer <- s.WithTx(context.Background(), func(storage.Storage) error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() {
		unblock()
		if err := <-writer; err != nil {
			t.Error(err)
		}
	}()
	const callers = 20
	reads := make(chan error, callers)
	for range callers {
		go func() {
			_, err := s.GetAgent(context.Background(), "missing")
			reads <- err
		}()
	}
	readDeadline := time.NewTimer(250 * time.Millisecond)
	defer readDeadline.Stop()
	for i := 0; i < callers; i++ {
		select {
		case err := <-reads:
			if !errors.Is(err, storage.ErrNotFound) {
				t.Errorf("read%d during the transaction = %v, want ErrNotFound", i, err)
			}
		case <-readDeadline.C:
			t.Fatal("reads waited for the active transaction")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := make(chan error, callers)
	ready := make(chan struct{}, callers)
	for range callers {
		go func() {
			ready <- struct{}{}
			writes <- s.WithTx(ctx, func(storage.Storage) error { return fmt.Errorf("canceled callback entered") })
		}()
	}
	for i := 0; i < callers; i++ {
		<-ready
	}
	cancel()
	writeDeadline := time.NewTimer(250 * time.Millisecond)
	defer writeDeadline.Stop()
	for i := 0; i < callers; i++ {
		select {
		case err := <-writes:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("writer%d canceled result=%v", i, err)
			}
		case <-writeDeadline.C:
			t.Fatal("canceled queued transactions waited for the active transaction")
		}
	}
	// The active transaction remains held; returning canceled callers did not abort it.
	select {
	case err := <-writer:
		writer <- err
		t.Fatalf("active transaction ended early:%v", err)
	default:
	}
}

func TestReadMethodsRejectCanceledContext(t *testing.T) {
	s, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]func() error{
		"agent":               func() error { _, e := s.GetAgent(ctx, "missing"); return e },
		"agents":              func() error { _, e := s.ListAgents(ctx, nil, storage.ListOptions{}); return e },
		"agent-credential":    func() error { _, e := s.GetAgentCredential(ctx, "missing"); return e },
		"user":                func() error { _, e := s.GetUser(ctx, "missing"); return e },
		"user-subject":        func() error { _, e := s.GetUserBySubject(ctx, "missing"); return e },
		"users":               func() error { _, e := s.ListUsers(ctx, nil, storage.ListOptions{}); return e },
		"credential":          func() error { _, e := s.GetCredential(ctx, "missing"); return e },
		"credential-auth":     func() error { _, e := s.GetCredentialByUserAndAuth(ctx, "missing", enum.Password); return e },
		"credentials":         func() error { _, e := s.ListCredentialsByUser(ctx, "missing"); return e },
		"verifier":            func() error { _, e := s.GetVerifier(ctx, "missing"); return e },
		"verifier-credential": func() error { _, e := s.GetVerifierByCredential(ctx, "missing"); return e },
		"session":             func() error { _, e := s.GetSession(ctx, "missing"); return e },
		"sessions":            func() error { _, e := s.ListSessionsByUser(ctx, "missing"); return e },
		"role":                func() error { _, e := s.GetRole(ctx, "missing"); return e },
		"roles-ids":           func() error { _, e := s.GetRoles(ctx, []string{"missing"}); return e },
		"role-name":           func() error { _, e := s.GetRoleByName(ctx, "missing"); return e },
		"roles":               func() error { _, e := s.ListRoles(ctx, nil, storage.ListOptions{}); return e },
		"spec":                func() error { _, e := s.GetSpec(ctx, "missing"); return e },
		"specs":               func() error { _, e := s.ListSpecs(ctx, nil, storage.ListOptions{}); return e },
		"rollout":             func() error { _, e := s.GetRollout(ctx, "missing"); return e },
		"rollouts":            func() error { _, e := s.ListRollouts(ctx, nil, storage.ListOptions{}); return e },
		"execution":           func() error { _, e := s.GetExecution(ctx, "missing"); return e },
		"executions":          func() error { _, e := s.ListExecutions(ctx, nil, storage.ListOptions{}); return e },
		"agents-scan":         func() error { _, e := s.ScanAgents(ctx, nil); return e },
		"rollouts-scan":       func() error { _, e := s.ScanRollouts(ctx, nil); return e },
		"specs-scan":          func() error { _, e := s.ScanSpecs(ctx, nil); return e },
		"executions-scan":     func() error { _, e := s.ScanExecutions(ctx, nil); return e },
	}
	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, context.Canceled) {
				t.Fatalf("expired context ignored:%v", err)
			}
		})
	}
}

func TestFilterConstructionDoesNotWaitForActiveTransaction(t *testing.T) {
	s, _ := newTestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithTx(context.Background(), func(storage.Storage) error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	built := make(chan struct{})
	go func() {
		s.BuildRolloutFilter(storage.RolloutQueryCriteria{AgentID: "a"})
		s.BuildSpecFilter(storage.SpecQueryCriteria{Query: "q"})
		s.BuildExecutionFilter(storage.ExecutionQueryCriteria{TaskUID: "uid", Statuses: []string{"failed"}})
		close(built)
	}()
	select {
	case <-built:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("criteria-only filter construction blocked on disk transaction")
	}
}

func TestCloseWaitsForActiveCommitAndIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- s.WithTx(context.Background(), func(storage.Storage) error { close(entered); <-release; return nil })
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("close abandoned active transaction:%v, writer:%v", err, <-writer)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAgent(context.Background(), "missing"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("closed store remained readable", err)
	}
}

// Close waits for a snapshot the background snapshotter is writing, which then
// commits whole.
func TestCloseWaitsForSnapshot(t *testing.T) {
	var (
		entered     = make(chan struct{})
		release     = make(chan struct{})
		enteredOnce sync.Once
		releaseOnce sync.Once
	)
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s, dir := newTestStore(t, WithSnapshotThreshold(1), withHooks(hooks{snapshot: func(p snapshotPoint) {
		if p == snapshotWritten {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
	}}))
	populate(t, s)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the commit that crossed the threshold started no snapshot")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned during the snapshot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l := readLayout(t, dir); !bytes.Equal(l.snapshotIndex, uint64Bytes(1)) || len(l.log) != 0 {
		t.Fatalf("snapshot index %x, log %v, want the snapshot at entry 1 and no log", l.snapshotIndex, l.log)
	}
}
