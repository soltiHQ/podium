package raft

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"
	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

func TestPreflightLogsSupportsCurrentCommandsAndSparseConfiguration(t *testing.T) {
	logs := hraft.NewInmemStore()
	data, err := encodeCommand([]*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.StoreLogs([]*hraft.Log{{Index: 1, Type: hraft.LogConfiguration, Data: []byte("configuration")}, {Index: 3, Type: hraft.LogCommand, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if err := preflightLogs(logs); err != nil {
		t.Fatal(err)
	}
	empty := hraft.NewInmemStore()
	if err := preflightLogs(empty); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightLogsRejectsLegacyAndMalformedCommands(t *testing.T) {
	legacy, err := proto.Marshal(&raftv1.Command{Ops: []*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := encodeCommand(nil)
	if err != nil {
		t.Fatal(err)
	}
	malformed := append(append([]byte(nil), current...), 0xff)
	for _, data := range [][]byte{legacy, malformed} {
		logs := hraft.NewInmemStore()
		if err := logs.StoreLog(&hraft.Log{Index: 1, Type: hraft.LogCommand, Data: data}); err != nil {
			t.Fatal(err)
		}
		if err := preflightLogs(logs); err == nil || !strings.Contains(err.Error(), "log 1") {
			t.Fatalf("incompatible command accepted: %v", err)
		}
	}
}

func TestPreflightSnapshotsRejectsLegacyBeforeFSM(t *testing.T) {
	for _, version := range []uint32{1, snapshot.Version} {
		store := hraft.NewInmemSnapshotStore()
		sink, err := store.Create(1, 1, 1, hraft.Configuration{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		var data []byte
		if version == snapshot.Version {
			data, err = snapshot.Marshal(inmemory.SnapshotContent{})
		} else {
			data, err = proto.Marshal(&raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: version}})
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sink.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		err = preflightSnapshots(store)
		if version == snapshot.Version && err != nil {
			t.Fatal(err)
		}
		if version != snapshot.Version && (err == nil || !strings.Contains(err.Error(), "unsupported version 1")) {
			t.Fatalf("legacy snapshot: %v", err)
		}
	}
}

func TestNewRejectsLegacyLogBeforeTransportAndWritableOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-log.bolt")
	logs, err := boltdb.NewBoltStore(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := proto.Marshal(&raftv1.Command{Ops: []*raftv1.Op{{Op: &raftv1.Op_AgentDelete{AgentDelete: "a1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.StoreLog(&hraft.Log{Index: 1, Term: 1, Type: hraft.LogCommand, Data: legacy}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inner := inmemory.New()
	a, _ := model.NewAgent("retain", "existing", "http://existing")
	if err := inner.UpsertAgent(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(zerolog.Nop())
	defer hub.Close()
	p, err := New(Config{NodeID: "n1", BindAddr: "127.0.0.1:0", DataDir: dir}, inner, hub, discovery.NewStatic(nil), zerolog.Nop())
	if p != nil {
		_ = p.Shutdown()
	}
	if err == nil || !strings.Contains(err.Error(), "preflight command at log 1") {
		t.Fatalf("legacy log reached transport/replay: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected legacy log database was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, "raft-stable.bolt")); !os.IsNotExist(err) {
		t.Fatal("writable stable store initialized before compatibility check")
	}
	if _, err := inner.GetAgent(context.Background(), "retain"); err != nil {
		t.Fatal("failed preflight mutated FSM state")
	}
	// Prove the failed check released its read lock; this write open is bounded.
	logs, err = boltdb.New(boltdb.Options{Path: path, BoltOptions: &bolt.Options{Timeout: 250 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	_ = logs.Close()
}

func TestNewRejectsLegacySnapshotBeforeTransportAndWritableOpen(t *testing.T) {
	dir := t.TempDir()
	store, err := hraft.NewFileSnapshotStore(dir, 2, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := store.Create(1, 1, 1, hraft.Configuration{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := proto.Marshal(&raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(legacy); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshots", sink.ID(), "state.bin")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(zerolog.Nop())
	defer hub.Close()
	p, err := New(Config{NodeID: "n1", BindAddr: "127.0.0.1:0", DataDir: dir}, inmemory.New(), hub, discovery.NewStatic(nil), zerolog.Nop())
	if p != nil {
		_ = p.Shutdown()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported version 1") {
		t.Fatalf("legacy snapshot reached transport/replay: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected legacy snapshot was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, "raft-log.bolt")); !os.IsNotExist(err) {
		t.Fatal("writable log store initialized before compatibility check")
	}
}
