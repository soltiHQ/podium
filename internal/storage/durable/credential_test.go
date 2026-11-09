package durable

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// credentialSecret is the enrollment secret of the tests below.
const credentialSecret = "0123456789abcdef0123456789abcdef"

// filesUnder returns the contents of every regular file under dir, by path.
func filesUnder(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// holding returns the files that hold token, as the hex text agents present or
// as the raw bytes it encodes.
func holding(files map[string][]byte, token string) []string {
	needles := [][]byte{[]byte(token)}
	if raw, err := hex.DecodeString(token); err == nil {
		needles = append(needles, raw)
	}
	var paths []string
	for path, data := range files {
		for _, needle := range needles {
			if bytes.Contains(data, needle) {
				paths = append(paths, path)
				break
			}
		}
	}
	return paths
}

// With the enrollment secret set, an agent's credential is derived, and no file
// under the data directory holds its token, as hex or as raw bytes: not the log
// entries of its enrollment and syncs, not the snapshot, not the entry after it.
// A token enrolled without the secret is stored, and the same search finds it,
// so the search sees what the store writes.
func TestDerivedCredentialStoresNoToken(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestStore(t)
	svc := agent.New(s, zerolog.Nop(), agent.WithEnrollSecret(credentialSecret))
	tokens := map[string]string{}
	enroll := func(id string) {
		t.Helper()
		token := agent.DeriveToken(credentialSecret, id, 0)
		for range 2 { // the enrolling sync, then a later one
			if err := svc.VerifyOrEnrollToken(ctx, id, token); err != nil {
				t.Fatalf("sync of %s: %v", id, err)
			}
		}
		tokens[id] = token
	}
	for i := range 4 {
		enroll(fmt.Sprintf("agent-%d", i))
	}
	const stored = "token-of-an-agent-enrolled-without-the-secret"
	if err := agent.New(s, zerolog.Nop()).VerifyOrEnrollToken(ctx, "tofu-agent", stored); err != nil {
		t.Fatal(err)
	}
	if err := s.writeSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	enroll("agent-after-the-snapshot")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	files := filesUnder(t, dir)
	for id, token := range tokens {
		if paths := holding(files, token); len(paths) != 0 {
			t.Errorf("the token of %s is in %v", id, paths)
		}
	}
	if paths := holding(files, stored); len(paths) == 0 {
		t.Fatal("the search found no stored token either: it cannot tell a token at rest")
	}
}

// relabelV3 turns the closed store in dir into one the previous version wrote:
// the snapshot and every log entry say version 3, the entries with their CRCs
// recomputed. The store must hold no derived credential, which version 3 cannot
// carry.
func relabelV3(t *testing.T, dir string) {
	t.Helper()
	mutateFile(t, dir, func(tx *bolt.Tx) error {
		if err := rewriteBlob(snapshotBucket, blobKey, func(msg *raftv1.Snapshot) { msg.Header.Version = 3 })(tx); err != nil {
			return err
		}
		log := tx.Bucket(logBucket)
		entries := map[uint64][]byte{}
		if err := log.ForEach(func(k, v []byte) error {
			index, _ := decodeUint64(k)
			command, err := decodeEntry(index, v)
			if err != nil {
				return err
			}
			command = append([]byte(nil), command...)
			command[len(overlay.CommandMagic)] = 3
			entries[index] = testEntry(index, command)
			return nil
		}); err != nil {
			return err
		}
		for index, entry := range entries {
			if err := log.Put(encodeUint64(index), entry); err != nil {
				return err
			}
		}
		return nil
	})
}

// versions returns the version of the snapshot and of each log entry of the
// closed store in dir.
func versions(t *testing.T, dir string) (snapshotVersion uint32, entryVersions []byte) {
	t.Helper()
	l := readLayout(t, dir)
	var msg raftv1.Snapshot
	if err := proto.Unmarshal(l.blob, &msg); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	err = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(logBucket).ForEach(func(_, v []byte) error {
			entryVersions = append(entryVersions, v[len(overlay.CommandMagic)])
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg.GetHeader().GetVersion(), entryVersions
}

// A store the previous version wrote, its snapshot and its log entries in
// version 3, opens with the state it holds, stored credentials as they were,
// whether the state sits in the log or in the snapshot. A derived credential
// written next joins the log in version 4, after the version-3 entries, and a
// restart replays both; the next snapshot is version 4.
func TestV3StoreLoads(t *testing.T) {
	for _, snapshotFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "state in the log", true: "state in the snapshot"}[snapshotFirst], func(t *testing.T) {
			ctx := context.Background()
			s, dir := newTestStore(t)
			populate(t, s)
			if snapshotFirst {
				if err := s.writeSnapshot(ctx); err != nil {
					t.Fatal(err)
				}
			}
			upsertAgent(t, s, "a2")
			want := snapshotBytes(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			relabelV3(t, dir)
			if v, entries := versions(t, dir); v != 3 || !bytes.Equal(entries, bytes.Repeat([]byte{3}, len(entries))) {
				t.Fatalf("the relabelled store: snapshot version %d, entries %v; want all 3", v, entries)
			}

			restored, err := Open(dir)
			if err != nil {
				t.Fatalf("open a version-3 store: %v", err)
			}
			if !bytes.Equal(snapshotBytes(t, restored), want) {
				_ = restored.Close()
				t.Fatal("the version-3 store opened with another state than it holds")
			}
			c, err := restored.GetAgentCredential(ctx, "a1")
			if err != nil || c.Derived() || c.Token() != "test-enrollment-token" {
				_ = restored.Close()
				t.Fatalf("the stored credential of a1: %v, want it stored with its token", err)
			}
			if err := agent.New(restored, zerolog.Nop(), agent.WithEnrollSecret(credentialSecret)).VerifyOrEnrollToken(ctx, "a3", agent.DeriveToken(credentialSecret, "a3", 0)); err != nil {
				_ = restored.Close()
				t.Fatal(err)
			}
			written := snapshotBytes(t, restored)
			if err := restored.Close(); err != nil {
				t.Fatal(err)
			}
			if _, entries := versions(t, dir); entries[len(entries)-1] != 4 {
				t.Fatalf("entry versions %v, want the new entry in version 4", entries)
			}

			again, err := Open(dir)
			if err != nil {
				t.Fatalf("open the mixed log: %v", err)
			}
			if !bytes.Equal(snapshotBytes(t, again), written) {
				_ = again.Close()
				t.Fatal("the mixed log replayed to another state")
			}
			if c, err := again.GetAgentCredential(ctx, "a3"); err != nil || !c.Derived() {
				_ = again.Close()
				t.Fatalf("the derived credential of a3: %v", err)
			}
			if err := again.writeSnapshot(ctx); err != nil {
				_ = again.Close()
				t.Fatal(err)
			}
			if err := again.Close(); err != nil {
				t.Fatal(err)
			}
			if v, _ := versions(t, dir); v != 4 {
				t.Fatalf("the next snapshot is version %d, want 4", v)
			}
		})
	}
}

// A store this version writes says version 4 in every frame of it, the header of
// its snapshot and the command of each log entry, whatever the frame holds: a
// stored credential and other rows as well as a derived credential. The
// previous version reads versions 2 and 3 alone (snapshot/codec.go and
// overlay/command.go before this change), so it refuses every frame of the store
// with its version error before it loads any state, and never reads a derived
// credential as a stored one. This version refuses a frame that says version 3
// and holds a derived credential, which no previous version wrote.
func TestV4StoreRefusedByV3Reader(t *testing.T) {
	ctx := context.Background()
	s, dir := newTestStore(t)
	populate(t, s)
	svc := agent.New(s, zerolog.Nop(), agent.WithEnrollSecret(credentialSecret))
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", agent.DeriveToken(credentialSecret, "agent-1", 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.writeSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	upsertAgent(t, s, "a2")
	if err := svc.VerifyOrEnrollToken(ctx, "agent-2", agent.DeriveToken(credentialSecret, "agent-2", 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	v, entries := versions(t, dir)
	if v != 4 || len(entries) != 2 || !bytes.Equal(entries, []byte{4, 4}) {
		t.Fatalf("snapshot version %d, entry versions %v; want version 4 throughout", v, entries)
	}

	mutateFile(t, dir, rewriteBlob(snapshotBucket, blobKey, func(msg *raftv1.Snapshot) { msg.Header.Version = 3 }))
	if restored, err := Open(dir); err == nil {
		_ = restored.Close()
		t.Fatal("a version-3 snapshot that holds a derived credential opened")
	}
}
