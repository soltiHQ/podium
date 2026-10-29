package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestQueuedReadsAndWritesCancelBeforeActiveTransactionEnds(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callers = 40
	replies := make(chan error, callers)
	ready := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			ready <- struct{}{}
			if i%2 == 0 {
				_, err := s.GetAgent(ctx, "missing")
				replies <- err
				return
			}
			replies <- s.WithTx(ctx, func(storage.Storage) error { return fmt.Errorf("canceled callback entered") })
		}(i)
	}
	for i := 0; i < callers; i++ {
		<-ready
	}
	cancel()
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	for i := 0; i < callers; i++ {
		select {
		case err := <-replies:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("caller%d canceled result=%v", i, err)
			}
		case <-deadline.C:
			t.Fatal("canceled queued calls waited for the active transaction")
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
