package durable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// singleBlobFixture is a database file of the single-blob layout, one bucket that
// holds the whole state, written by Open and the transactions of populate,
// RotateRefreshAt, RevokeSession and UpsertExecution at commit efc821c, before the
// log. It holds every entity group.
const singleBlobFixture = "testdata/single-blob.db"

// singleBlobStore copies the single-blob fixture into a fresh private data
// directory and returns the directory.
func singleBlobStore(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(singleBlobFixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// singleBlob returns the snapshot a single-blob database file in dir holds, after
// checking its checksum.
func singleBlob(t *testing.T, dir string) []byte {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var blob []byte
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(legacyBucket)
		if b == nil {
			return errors.New("no single-blob bucket")
		}
		blob = slices.Clone(b.Get(legacySnapshotKey))
		sum := sha256.Sum256(blob)
		if !bytes.Equal(b.Get(checksumKey), sum[:]) {
			return errors.New("single-blob checksum mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return blob
}

// atCurrentVersion is the version-3 snapshot blob, as the fixture holds it, with
// the current version in its header: what the migration of the fixture writes.
func atCurrentVersion(t *testing.T, blob []byte) []byte {
	t.Helper()
	var msg raftv1.Snapshot
	if err := proto.Unmarshal(blob, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.GetHeader().GetVersion() != 3 {
		t.Fatalf("the fixture's snapshot is version %d, want 3", msg.GetHeader().GetVersion())
	}
	msg.Header.Version = snapshot.Version
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A single-blob file opens with the state it holds, now in the log layout: the
// snapshot at index 0, in the current version, and an empty log. A write and a
// restart then keep it. The fixture's snapshot is version 3: the migrated one
// is the same snapshot with version 4 in its header, its stored credentials
// stored still.
func TestOpenMigratesSingleBlobStore(t *testing.T) {
	dir := singleBlobStore(t)
	legacy := singleBlob(t, dir)
	content, err := snapshot.Unmarshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Agents) == 0 || len(content.AgentCreds) == 0 || len(content.Users) == 0 || len(content.Roles) == 0 || len(content.Credentials) == 0 ||
		len(content.Verifiers) == 0 || len(content.Sessions) == 0 || len(content.Specs) == 0 || len(content.Rollouts) == 0 || len(content.Executions) == 0 {
		t.Fatal("the fixture must hold every entity group")
	}
	want := atCurrentVersion(t, legacy)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshotBytes(t, s), want) {
		_ = s.Close()
		t.Fatal("the migrated state differs from the single-blob snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	l := readLayout(t, dir)
	if !slices.Equal(l.buckets, []string{"log", "meta", "snapshot"}) || !bytes.Equal(l.format, uint64Bytes(2)) ||
		!bytes.Equal(l.snapshotIndex, uint64Bytes(0)) || len(l.log) != 0 || !bytes.Equal(l.blob, want) {
		t.Fatalf("after the migration: buckets %v, format %x, snapshot index %x, log %v; want the log layout at format 2 with the snapshot at index 0 and no log",
			l.buckets, l.format, l.snapshotIndex, l.log)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	upsertAgent(t, s, "a2")
	written := snapshotBytes(t, s)
	assertReopen(t, s, dir, written)
	if l := readLayout(t, dir); !slices.Equal(l.log, []uint64{1}) || !bytes.Equal(l.blob, want) {
		t.Fatalf("after a write: log %v, want entry 1 over the migrated snapshot", l.log)
	}
}

// Open says whether it migrated the file, so that its caller can warn: a
// single-blob file is migrated once. The migrated file is in format 2, and so is
// a new one, and opening either reports nothing.
func TestOpenReportsOnlyAMigration(t *testing.T) {
	dir := singleBlobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Migrated() {
		t.Error("Open of the single-blob fixture did not report the migration")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Migrated() {
		t.Error("Open of the migrated file, which is in format 2, reported a migration")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Migrated() {
		t.Error("Open of a new directory reported a migration")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

// A single-blob file that does not check out stops Open before the migration,
// which leaves the file as it was.
func TestOpenRejectsCorruptSingleBlobStore(t *testing.T) {
	rewrite := func(change func(*raftv1.Snapshot)) func(*bolt.Tx) error {
		return rewriteBlob(legacyBucket, legacySnapshotKey, change)
	}
	tests := []struct {
		name    string
		corrupt func(*bolt.Tx) error
	}{
		{"checksum", func(tx *bolt.Tx) error { return tx.Bucket(legacyBucket).Put(checksumKey, []byte("bad checksum")) }},
		{"header", rewrite(func(msg *raftv1.Snapshot) { msg.Header.Version = 99 })},
		{"version 2", rewrite(func(msg *raftv1.Snapshot) { msg.Header.Version, msg.Executions = 2, nil })},
		{"duplicate id", rewrite(func(msg *raftv1.Snapshot) { msg.Agents = append(msg.Agents, msg.Agents[0]) })},
		{"missing snapshot", func(tx *bolt.Tx) error { return tx.Bucket(legacyBucket).Delete(legacySnapshotKey) }},
		{"next to the log layout", func(tx *bolt.Tx) error {
			_, err := tx.CreateBucket(metaBucket)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := singleBlobStore(t)
			mutateFile(t, dir, test.corrupt)
			path := filepath.Join(dir, FileName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if restored, err := Open(dir); err == nil {
				_ = restored.Close()
				t.Fatal("corrupt single-blob state accepted")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("a refused migration changed the database file: %v", err)
			}
		})
	}
}

// singleBlobFile writes a single-blob database file that holds blob into a fresh
// private data directory and returns the directory.
func singleBlobFile(t *testing.T, blob []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket(legacyBucket)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(blob)
		if err := b.Put(legacySnapshotKey, blob); err != nil {
			return err
		}
		return b.Put(checksumKey, sum[:])
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A database file bbolt initialized without a bucket, as a crash right after its
// creation leaves it, opens as a fresh store.
func TestOpenTreatsEmptyFileAsFresh(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, FileName), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a database with no bucket: %v", err)
	}
	if _, err := s.GetAgent(context.Background(), "a1"); !errors.Is(err, storage.ErrNotFound) {
		_ = s.Close()
		t.Fatalf("fresh store: %v, want ErrNotFound", err)
	}
	populate(t, s)
	assertReopen(t, s, dir, snapshotBytes(t, s))
	if l := readLayout(t, dir); !slices.Equal(l.buckets, []string{"log", "meta", "snapshot"}) || !slices.Equal(l.log, []uint64{1}) {
		t.Fatalf("buckets %v, log %v, want the log layout with entry 1", l.buckets, l.log)
	}
}
