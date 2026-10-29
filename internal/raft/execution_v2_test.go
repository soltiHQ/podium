package raft

import (
	"context"
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestRaftVersion2CommandLogCanBeReadAndExtended(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p, _, hub := openRestartProfile(t, dir)
	a, err := model.NewAgent("v2-agent", "Before archive", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store().UpsertAgent(ctx, a); err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Shutdown(); err != nil {
		hub.Close()
		t.Fatal(err)
	}
	hub.Close()
	// Convert only this test-owned old-entity command to the exact previous
	// prefix; its protobuf bytes are unchanged and contain no archive entity.
	logs, err := boltdb.NewBoltStore(filepath.Join(dir, "raft-log.bolt"))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := logs.FirstIndex()
	last, _ := logs.LastIndex()
	converted := 0
	for i := first; i <= last; i++ {
		var entry hraft.Log
		if err := logs.GetLog(i, &entry); err != nil {
			_ = logs.Close()
			t.Fatal(err)
		}
		if entry.Type != hraft.LogCommand {
			continue
		}
		if _, err := decodeCommand(entry.Data); err != nil {
			_ = logs.Close()
			t.Fatal(err)
		}
		entry.Data = append([]byte(nil), entry.Data...)
		entry.Data[len(commandMagic)] = 2
		if err := logs.StoreLog(&entry); err != nil {
			_ = logs.Close()
			t.Fatal(err)
		}
		converted++
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	if converted == 0 {
		t.Fatal("fixture had no previous-format command")
	}
	restored, _, rhub := openRestartProfile(t, dir)
	e := raftArchiveRow(t, 1)
	if err := restored.Store().WithTx(ctx, func(tx storage.Storage) error {
		if _, err := tx.GetAgent(ctx, a.ID()); err != nil {
			return err
		}
		return tx.UpsertExecution(ctx, e)
	}); err != nil {
		_ = restored.Shutdown()
		rhub.Close()
		t.Fatal("v2 replay failed", err)
	}
	if err := restored.Shutdown(); err != nil {
		rhub.Close()
		t.Fatal(err)
	}
	rhub.Close()
	again, inner, againHub := openRestartProfile(t, dir)
	defer func() { _ = again.Shutdown() }()
	defer againHub.Close()
	if err := cluster.AwaitLeaderReady(ctx, again.Leadership()); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.GetAgent(ctx, a.ID()); err != nil {
		t.Fatal("previous state lost after adding archive", err)
	}
	if _, err := inner.GetExecution(ctx, e.ID()); err != nil {
		t.Fatal("mixed v2/v3 log archive lost", err)
	}
}
