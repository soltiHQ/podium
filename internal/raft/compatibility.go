package raft

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	bolt "go.etcd.io/bbolt"
)

// The read-only open does not initialize buckets or rewrite database metadata.
// Legacy or corrupt commands are rejected before NewRaft can replay them. The
// writable log handle is rechecked after acquiring its exclusive lock.
func preflightLogFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("raft: preflight log file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("raft: preflight log file must be a nonempty regular file")
	}
	logs, err := boltdb.New(boltdb.Options{Path: path, BoltOptions: &bolt.Options{ReadOnly: true, Timeout: 250 * time.Millisecond}})
	if err != nil {
		return fmt.Errorf("raft: preflight open log: %w", err)
	}
	defer logs.Close()
	return preflightLogs(logs)
}

func preflightLogs(logs hraft.LogStore) error {
	first, err := logs.FirstIndex()
	if err != nil {
		return fmt.Errorf("raft: preflight first log: %w", err)
	}
	last, err := logs.LastIndex()
	if err != nil {
		return fmt.Errorf("raft: preflight last log: %w", err)
	}
	if first == 0 && last == 0 {
		return nil
	}
	if first == 0 || first > last {
		return fmt.Errorf("raft: preflight invalid log bounds %d..%d", first, last)
	}
	for index := first; ; index++ {
		var entry hraft.Log
		err := logs.GetLog(index, &entry)
		// Raft's LogStore contract explicitly permits gaps after snapshot restore.
		if err != nil && !errors.Is(err, hraft.ErrLogNotFound) {
			return fmt.Errorf("raft: preflight read log %d: %w", index, err)
		}
		if err == nil && entry.Type == hraft.LogCommand {
			if _, err := decodeCommand(entry.Data); err != nil {
				return fmt.Errorf("raft: preflight command at log %d: %w", index, err)
			}
		}
		if index == last {
			break
		}
	}
	return nil
}

// Validate every snapshot Raft may use as a restore fallback before launching
// its FSM. SnapshotStore.Open also verifies the library's physical checksum.
// A failed validation closes the reader and never mutates live domain state.
func preflightSnapshots(store hraft.SnapshotStore) error {
	metas, err := store.List()
	if err != nil {
		return fmt.Errorf("raft: preflight list snapshots: %w", err)
	}
	for _, meta := range metas {
		_, reader, err := store.Open(meta.ID)
		if err != nil {
			return fmt.Errorf("raft: preflight open snapshot %q: %w", meta.ID, err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return fmt.Errorf("raft: preflight read snapshot %q: %w", meta.ID, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("raft: preflight close snapshot %q: %w", meta.ID, closeErr)
		}
		if _, err := snapshot.Unmarshal(data); err != nil {
			return fmt.Errorf("raft: preflight snapshot %q: %w", meta.ID, err)
		}
	}
	return nil
}
