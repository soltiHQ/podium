package raft

import (
	"context"
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// A raft log the previous version wrote, its commands in version 3, replays to
// the state it holds, a stored credential as it was, and takes the commands of
// this version after it: a derived credential, in version 4. A restart replays
// the mixed log.
func TestRaftVersion3CommandLogCanBeReadAndExtended(t *testing.T) {
	ctx := context.Background()
	dir := privateTestDir(t)
	p, _, hub := openRestartProfile(t, dir)
	a, err := model.NewAgent("v3-agent", "Before derived credentials", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := model.NewStoredAgentCredential("v3-agent", "stored-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store().WithTx(ctx, func(tx storage.Storage) error {
		if err := tx.UpsertAgent(ctx, a); err != nil {
			return err
		}
		return tx.UpsertAgentCredential(ctx, cred)
	}); err != nil {
		_ = p.Shutdown()
		hub.Close()
		t.Fatal(err)
	}
	if err := p.Shutdown(); err != nil {
		hub.Close()
		t.Fatal(err)
	}
	hub.Close()
	// Turn this test's own commands into those of the previous version: their
	// protobuf bytes stay as they are, and hold no derived credential.
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
		if _, err := overlay.DecodeCommand(entry.Data); err != nil {
			_ = logs.Close()
			t.Fatal(err)
		}
		entry.Data = append([]byte(nil), entry.Data...)
		entry.Data[len(overlay.CommandMagic)] = 3
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
		t.Fatal("the log had no command to convert")
	}

	restored, _, rhub := openRestartProfile(t, dir)
	svc := agent.New(restored.Store(), zerolog.Nop(), agent.WithEnrollSecret(credentialSecret))
	if err := svc.VerifyOrEnrollToken(ctx, "v3-agent", "stored-token"); err != nil {
		_ = restored.Shutdown()
		rhub.Close()
		t.Fatalf("the stored token after the version-3 replay: %v", err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-1", agent.DeriveToken(credentialSecret, "agent-1", 0)); err != nil {
		_ = restored.Shutdown()
		rhub.Close()
		t.Fatal("enrolling after the version-3 replay failed", err)
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
		t.Fatal("the state of the version-3 log is lost", err)
	}
	if c, err := inner.GetAgentCredential(ctx, "v3-agent"); err != nil || c.Derived() || c.Token() != "stored-token" {
		t.Fatalf("the stored credential after the mixed replay: %v, want it stored with its token", err)
	}
	if c, err := inner.GetAgentCredential(ctx, "agent-1"); err != nil || !c.Derived() {
		t.Fatalf("the derived credential after the mixed replay: %v", err)
	}
}
