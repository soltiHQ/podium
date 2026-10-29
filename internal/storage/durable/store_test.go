package durable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

func populate(t *testing.T, s storage.Storage) string {
	t.Helper()
	ctx := context.Background()
	a, err := model.NewAgent("a1", "agent-one", "http://127.0.0.1:9000")
	if err != nil {
		t.Fatal(err)
	}
	a.LabelAdd("env", "test")
	a.SetOS("linux")
	a.SetCapabilities([]model.RunnerCapability{{Name: "echo", Labels: map[string]string{"tier": "test"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Shell"}}}})
	ac, err := model.NewAgentCredential("a1", "test-enrollment-token")
	if err != nil {
		t.Fatal(err)
	}
	u, err := model.NewUser("u1", "subject-one")
	if err != nil {
		t.Fatal(err)
	}
	u.SetName("Test user")
	r, err := model.NewRole("r1", "test-role")
	if err != nil {
		t.Fatal(err)
	}
	c, err := model.NewCredential("c1", "u1", enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	v, err := model.NewVerifier("v1", "c1", enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	session, err := model.NewSession("s1", "u1", "c1", enum.Password, []byte("test-hash"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := model.NewSpec("sp1", "test-spec", "test-slot")
	if err != nil {
		t.Fatal(err)
	}
	spec.SetKindConfig(map[string]any{"args": []string{"echo", "hello"}, "nested": map[string]string{"key": "value"}})
	spec.SetGeneration(5)
	spec.SetDeletionRequested(true)
	rollout, err := model.NewRollout("sp1", "a1", 5)
	if err != nil {
		t.Fatal(err)
	}
	rollout.SetActualTaskID("installed-task")
	rollout.RestoreState(enum.SyncStatusFailed, enum.RolloutIntentUninstall, 4, 3, "retry after failure", time.Now().Add(-time.Hour), time.Now().Add(-time.Minute))
	err = s.WithTx(ctx, func(tx storage.Storage) error {
		for _, call := range []func() error{
			func() error { return tx.UpsertAgent(ctx, a) },
			func() error { return tx.UpsertAgentCredential(ctx, ac) },
			func() error { return tx.UpsertUser(ctx, u) },
			func() error { return tx.UpsertRole(ctx, r) },
			func() error { return tx.UpsertCredential(ctx, c) },
			func() error { return tx.UpsertVerifier(ctx, v) },
			func() error { return tx.CreateSession(ctx, session) },
			func() error { return tx.UpsertSpec(ctx, spec) },
			func() error { return tx.UpsertRollout(ctx, rollout) },
		} {
			if err := call(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rollout.ID()
}

func snapshotBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := snapshot.Marshal(s.inner.SnapshotForRaft())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertReopen(t *testing.T, s *Store, dir string, expected []byte) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if !bytes.Equal(expected, snapshotBytes(t, restored)) {
		t.Fatal("committed state differs after reopening")
	}
}

func TestRoundTripAllEntities(t *testing.T) {
	s, dir := newTestStore(t)
	rolloutID := populate(t, s)
	ctx := context.Background()
	if err := s.RotateRefresh(ctx, "s1", []byte("rotated-hash"), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "s1", time.Now()); err != nil {
		t.Fatal(err)
	}
	content := s.inner.SnapshotForRaft()
	if len(content.Agents) != 1 || len(content.AgentCreds) != 1 || len(content.Users) != 1 || len(content.Roles) != 1 || len(content.Credentials) != 1 || len(content.Verifiers) != 1 || len(content.Sessions) != 1 || len(content.Specs) != 1 || len(content.Rollouts) != 1 {
		t.Fatal("fixture must cover all nine aggregates")
	}
	if _, err := s.GetRollout(ctx, rolloutID); err != nil {
		t.Fatal(err)
	}
	assertReopen(t, s, dir, snapshotBytes(t, s))
}

func TestEveryMutationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*Store, string) error
	}{
		{"agent upsert", func(s *Store, _ string) error {
			a, _ := model.NewAgent("a2", "second", "http://second")
			return s.UpsertAgent(ctx, a)
		}},
		{"agent delete", func(s *Store, _ string) error { return s.DeleteAgent(ctx, "a1") }},
		{"agent credential upsert", func(s *Store, _ string) error {
			c, _ := model.NewAgentCredential("a1", "rotated-token")
			return s.UpsertAgentCredential(ctx, c)
		}},
		{"agent credential delete", func(s *Store, _ string) error { return s.DeleteAgentCredential(ctx, "a1") }},
		{"user upsert", func(s *Store, _ string) error {
			u, _ := model.NewUser("u2", "subject-two")
			return s.UpsertUser(ctx, u)
		}},
		{"user delete", func(s *Store, _ string) error { return s.DeleteUser(ctx, "u1") }},
		{"credential upsert", func(s *Store, _ string) error {
			c, _ := model.NewCredential("c2", "u1", enum.Password)
			return s.UpsertCredential(ctx, c)
		}},
		{"credential delete", func(s *Store, _ string) error { return s.DeleteCredential(ctx, "c1") }},
		{"verifier upsert", func(s *Store, _ string) error {
			v, _ := model.NewVerifier("v2", "c2", enum.Password)
			return s.UpsertVerifier(ctx, v)
		}},
		{"verifier delete", func(s *Store, _ string) error { return s.DeleteVerifier(ctx, "v1") }},
		{"verifier delete by credential", func(s *Store, _ string) error { return s.DeleteVerifierByCredential(ctx, "c1") }},
		{"session create", func(s *Store, _ string) error {
			v, _ := model.NewSession("s2", "u1", "c1", enum.Password, []byte("hash2"), time.Now().Add(time.Hour))
			return s.CreateSession(ctx, v)
		}},
		{"session rotate", func(s *Store, _ string) error {
			return s.RotateRefresh(ctx, "s1", []byte("hash2"), time.Now().Add(2*time.Hour))
		}},
		{"session revoke", func(s *Store, _ string) error { return s.RevokeSession(ctx, "s1", time.Now()) }},
		{"session delete", func(s *Store, _ string) error { return s.DeleteSession(ctx, "s1") }},
		{"sessions delete by user", func(s *Store, _ string) error { return s.DeleteSessionsByUser(ctx, "u1") }},
		{"role upsert", func(s *Store, _ string) error {
			v, _ := model.NewRole("r2", "second-role")
			return s.UpsertRole(ctx, v)
		}},
		{"role delete", func(s *Store, _ string) error { return s.DeleteRole(ctx, "r1") }},
		{"spec upsert", func(s *Store, _ string) error {
			v, _ := model.NewSpec("sp2", "second-spec", "second-slot")
			return s.UpsertSpec(ctx, v)
		}},
		{"spec delete", func(s *Store, _ string) error { return s.DeleteSpec(ctx, "sp1") }},
		{"rollout upsert", func(s *Store, _ string) error {
			v, _ := model.NewRollout("sp1", "a2", 1)
			return s.UpsertRollout(ctx, v)
		}},
		{"rollout delete", func(s *Store, id string) error { return s.DeleteRollout(ctx, id) }},
		{"rollouts delete by spec", func(s *Store, _ string) error { return s.DeleteRolloutsBySpec(ctx, "sp1") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s, dir := newTestStore(t)
			id := populate(t, s)
			before := snapshotBytes(t, s)
			if err := test.mutate(s, id); err != nil {
				t.Fatal(err)
			}
			after := snapshotBytes(t, s)
			if bytes.Equal(before, after) {
				t.Fatal("mutation did not change state")
			}
			assertReopen(t, s, dir, after)
		})
	}
}

func TestRollbackCancellationAndCommitFailure(t *testing.T) {
	s, dir := newTestStore(t)
	populate(t, s)
	before := snapshotBytes(t, s)
	ctx := context.Background()
	sentinel := errors.New("callback failed")
	if err := s.WithTx(ctx, func(tx storage.Storage) error { _ = tx.DeleteAgent(ctx, "a1"); return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback error: %v", err)
	}
	if !bytes.Equal(before, snapshotBytes(t, s)) {
		t.Fatal("failed callback published changes")
	}
	cancelled, cancel := context.WithCancel(ctx)
	if err := s.WithTx(cancelled, func(tx storage.Storage) error {
		if err := tx.DeleteAgent(ctx, "a1"); err != nil {
			return err
		}
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if !bytes.Equal(before, snapshotBytes(t, s)) {
		t.Fatal("cancelled callback published changes")
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, "a1"); !errors.Is(err, storage.ErrInternal) {
		t.Fatalf("disk commit failure: %v", err)
	}
	if !bytes.Equal(before, snapshotBytes(t, s)) {
		t.Fatal("failed disk commit published changes")
	}
	if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("disk failure must fail closed: %v", err)
	}
	if err := s.DeleteAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("writes after disk failure must fail closed: %v", err)
	}
	assertReopen(t, s, dir, before)
}

func TestNestedTransactionAndClosedStore(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	if err := s.WithTx(ctx, func(tx storage.Storage) error {
		return tx.WithTx(ctx, func(nested storage.Storage) error {
			a, _ := model.NewAgent("a1", "nested", "http://nested")
			return nested.UpsertAgent(ctx, a)
		})
	}); err != nil {
		t.Fatal(err)
	}
	expected := snapshotBytes(t, s)
	assertReopen(t, s, dir, expected)
	if err := s.DeleteAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("closed write: %v", err)
	}
	if _, err := s.GetAgent(ctx, "a1"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("closed read: %v", err)
	}
	if err := s.WithTx(ctx, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("nil callback: %v", err)
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errorsCh := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			id := fmt.Sprintf("a-%02d", i)
			a, _ := model.NewAgent(id, id, "http://agent")
			if err := s.UpsertAgent(ctx, a); err != nil {
				errorsCh <- err
				return
			}
			if _, err := s.GetAgent(ctx, id); err != nil {
				errorsCh <- err
			}
		})
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	page, err := s.ListAgents(ctx, nil, storage.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 16 {
		t.Fatalf("writers lost state: count=%v", len(page.Items))
	}
	assertReopen(t, s, dir, snapshotBytes(t, s))
}

func TestUncommittedCandidateIsNotVisible(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	changed := make(chan struct{})
	release := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- s.WithTx(ctx, func(tx storage.Storage) error {
			a, _ := model.NewAgent("uncommitted", "candidate", "http://candidate")
			if err := tx.UpsertAgent(ctx, a); err != nil {
				return err
			}
			close(changed)
			<-release
			return errors.New("rollback")
		})
	}()
	<-changed
	readDone := make(chan error, 1)
	go func() { _, err := s.GetAgent(ctx, "uncommitted"); readDone <- err }()
	select {
	case err := <-readDone:
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("uncommitted candidate observed: %v", err)
		}
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-txDone; err == nil {
		t.Fatal("expected rollback")
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader stuck after rollback")
	}
}

func TestProcessLockAndPrivatePermissions(t *testing.T) {
	s, dir := newTestStore(t)
	start := time.Now()
	second, err := Open(dir)
	if second != nil {
		_ = second.Close()
	}
	if err == nil {
		t.Fatal("second process handle accepted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("database lock wait was not bounded")
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file permissions: %o", info.Mode().Perm())
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions: %o", info.Mode().Perm())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = Open(dir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = second.Close()
	if err := os.Chmod(filepath.Join(dir, FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if second, err := Open(dir); err == nil {
		_ = second.Close()
		t.Fatal("insecure existing permissions accepted")
	}
}

func TestCorruptStateIsRejectedWithoutReset(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*bolt.Tx) error
	}{
		{"checksum", func(tx *bolt.Tx) error { return tx.Bucket(stateBucket).Put(checksumKey, []byte("bad checksum")) }},
		{"header", func(tx *bolt.Tx) error {
			b := tx.Bucket(stateBucket)
			var msg raftv1.Snapshot
			if err := proto.Unmarshal(b.Get(snapshotKey), &msg); err != nil {
				return err
			}
			msg.Header.Version = 99
			data, err := proto.Marshal(&msg)
			if err != nil {
				return err
			}
			return putSnapshot(b, data)
		}},
		{"duplicate id", func(tx *bolt.Tx) error {
			b := tx.Bucket(stateBucket)
			var msg raftv1.Snapshot
			if err := proto.Unmarshal(b.Get(snapshotKey), &msg); err != nil {
				return err
			}
			msg.Agents = append(msg.Agents, msg.Agents[0])
			data, err := proto.Marshal(&msg)
			if err != nil {
				return err
			}
			return putSnapshot(b, data)
		}},
		{"missing snapshot", func(tx *bolt.Tx) error { return tx.Bucket(stateBucket).Delete(snapshotKey) }},
		{"missing bucket", func(tx *bolt.Tx) error { return tx.DeleteBucket(stateBucket) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, dir := newTestStore(t)
			populate(t, s)
			_ = s.Close()
			path := filepath.Join(dir, FileName)
			db, err := bolt.Open(path, 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(test.corrupt); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if restored, err := Open(dir); err == nil {
				_ = restored.Close()
				t.Fatal("corrupt state accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed restore modified database")
			}
		})
	}
}

func TestInvalidExistingFileIsPreserved(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not a bbolt database")} {
		t.Run(fmt.Sprintf("size-%d", len(data)), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if restored, err := Open(dir); err == nil {
				_ = restored.Close()
				t.Fatal("invalid database accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, after) {
				t.Fatal("invalid file overwritten")
			}
		})
	}
	if _, err := Open(" "); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatal("blank data directory accepted")
	}
}

// os.Exit skips Close and all deferred cleanup, as an abruptly exited process
// does. The parent verifies that an acknowledged commit was already durable.
func TestAcknowledgedWritesSurviveProcessExit(t *testing.T) {
	if dir := os.Getenv("SOLTI_DURABLE_TEST_CHILD_DIR"); dir != "" {
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		populate(t, s)
		os.Exit(0)
	}
	dir := filepath.Join(t.TempDir(), "crash-state")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(binary, "-test.run=^TestAcknowledgedWritesSurviveProcessExit$")
	child.Env = append(os.Environ(), "SOLTI_DURABLE_TEST_CHILD_DIR="+dir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	restored, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	content := restored.inner.SnapshotForRaft()
	if len(content.Agents)+len(content.AgentCreds)+len(content.Users)+len(content.Roles)+len(content.Credentials)+len(content.Verifiers)+len(content.Sessions)+len(content.Specs)+len(content.Rollouts) != 9 {
		t.Fatal("acknowledged state lost after process exit")
	}
}

func TestCandidateDoesNotAliasPreviousState(t *testing.T) {
	s, _ := newTestStore(t)
	populate(t, s)
	old := s.inner
	ctx := context.Background()
	if err := s.WithTx(ctx, func(tx storage.Storage) error {
		a, err := tx.GetAgent(ctx, "a1")
		if err != nil {
			return err
		}
		a.SetOS("darwin")
		return tx.UpsertAgent(ctx, a)
	}); err != nil {
		t.Fatal(err)
	}
	if a, err := old.GetAgent(ctx, "a1"); err != nil || a.OS() != "linux" {
		t.Fatal("candidate mutated previous snapshot")
	}
}

func TestInvalidSpecConfigDoesNotPublishOrPersist(t *testing.T) {
	s, dir := newTestStore(t)
	before := snapshotBytes(t, s)
	spec, err := model.NewSpec("bad", "invalid-config", "slot")
	if err != nil {
		t.Fatal(err)
	}
	spec.SetKindConfig(map[string]any{"unsupported": make(chan string)})
	if err := s.UpsertSpec(context.Background(), spec); err == nil {
		t.Fatal("invalid configuration silently persisted")
	}
	if !bytes.Equal(before, snapshotBytes(t, s)) {
		t.Fatal("failed encoding published candidate")
	}
	if _, err := s.GetSpec(context.Background(), "bad"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed candidate visible: %v", err)
	}
	assertReopen(t, s, dir, before)
}
