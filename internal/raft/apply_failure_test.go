package raft

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// failedCommands returns the lines of logs that report a committed command the
// node could not decode or apply.
func failedCommands(t *testing.T, logs *logBuffer) []map[string]any {
	t.Helper()
	var failed []map[string]any
	for _, line := range logLines(t, logs) {
		if message, _ := line["message"].(string); strings.HasPrefix(message, "raft: committed command") {
			failed = append(failed, line)
		}
	}
	return failed
}

// A committed command that cannot be decoded, or that fails to apply, is
// dropped whole by every replica, and every replica logs it at ERROR: its kind,
// the number of its operations and the index of its log entry, never what it
// carries. So a corrupted entry or a version skew is not silent on the
// followers, where nobody reads the error that Apply returns to the proposing
// leader. The commands are injected into the log directly, past the validation
// that Store applies when it proposes.
func TestFollowerLogsFailedApply(t *testing.T) {
	const marker = "marker-4711" // in the value of every injected command
	agent, err := model.NewAgent("rolled-back", "name "+marker, "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	upsert := &raftv1.Op{Op: &raftv1.Op_AgentUpsert{AgentUpsert: wire.AgentToProto(agent)}}
	missing := &raftv1.Op{Op: &raftv1.Op_AgentDelete{AgentDelete: "missing-" + marker}}
	transaction, err := overlay.EncodeCommand([]*raftv1.Op{upsert, missing})
	if err != nil {
		t.Fatal(err)
	}

	c := loopbackNodes(t, "n1", "n2", "n3")
	logs := make(map[string]*logBuffer, len(c.nodes))
	for _, n := range c.nodes {
		logs[n.id] = n.capture()
		n.start(t, c.discovery(t))
	}
	leader := c.leader(t, 10*time.Second)

	for i, tc := range []struct {
		name    string
		data    []byte
		message string
		kind    string
		ops     int // of a command that decodes; 0 for one that does not
	}{
		{
			"an entry that does not decode", []byte("not a command " + marker),
			"raft: committed command is not decodable and was dropped on this replica (corrupted entry or version skew)", "undecodable", 0,
		},
		{
			"a transaction whose second operation fails", transaction,
			"raft: committed command failed to apply and was dropped on this replica, which stays as it was", "agent_upsert,agent_delete", 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The leader's caller alone gets the error, as the response of Apply.
			future := leader.profile.raft.Apply(tc.data, 5*time.Second)
			if err := future.Error(); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, ok := future.Response().(error); !ok {
				t.Fatalf("the leader's Apply response is %v, want the error of the FSM", future.Response())
			}

			var index any
			for _, n := range c.nodes {
				waitFor(t, 5*time.Second, n.id+" logs the failed command", func() bool { return len(failedCommands(t, logs[n.id])) > i })
				failed := failedCommands(t, logs[n.id])
				if len(failed) != i+1 {
					t.Fatalf("%s logged %d failed commands, want %d: %s", n.id, len(failed), i+1, logs[n.id])
				}
				entry := failed[i]
				if entry["level"] != "error" || entry["message"] != tc.message || entry["profile"] != "raft" || entry["kind"] != tc.kind {
					t.Fatalf("%s logged %v, want an ERROR %q of kind %q", n.id, entry, tc.message, tc.kind)
				}
				if ops, logged := entry["ops"].(float64); logged != (tc.ops != 0) || int(ops) != tc.ops {
					t.Fatalf("%s logged %v operations: %v, want %d", n.id, entry["ops"], entry, tc.ops)
				}
				// Every replica reports the same entry of the log.
				if index == nil {
					index = entry["index"]
				}
				if got, ok := entry["index"].(float64); !ok || got <= 0 || got != index {
					t.Fatalf("%s logged entry %v, want %v: %v", n.id, entry["index"], index, entry)
				}
			}
		})
	}

	ctx := context.Background()
	for _, n := range c.nodes {
		if strings.Contains(logs[n.id].String(), marker) {
			t.Fatalf("%s logged a value of a command: %s", n.id, logs[n.id])
		}
		// The failed transaction changed nothing anywhere, its first operation included.
		if _, err := n.profile.inner.GetAgent(ctx, agent.ID()); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("%s holds the agent of a command that failed: %v", n.id, err)
		}
	}

	// A command that fails is no poison: the next write replicates.
	next, err := model.NewAgent("next", "next", "http://next")
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, 10*time.Second, func(s storage.Storage) error { return s.UpsertAgent(ctx, next) })
	for _, n := range c.nodes {
		waitFor(t, 5*time.Second, n.id+" applies the next write", func() bool {
			_, err := n.profile.inner.GetAgent(ctx, next.ID())
			return err == nil
		})
	}
}
