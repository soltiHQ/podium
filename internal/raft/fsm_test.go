package raft

import (
	"context"
	"errors"
	"strings"
	"testing"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// Apply answers the error of a command that cannot be decoded or fails to
// apply, which the leader's caller gets, drops the command whole, and logs one
// ERROR line with its log index, its kind and its number of operations, never
// its error or what it carries. A command that applies, one that is only events
// included, logs nothing.
func TestFSMApplyLogsOnlyTheShapeOfAFailedCommand(t *testing.T) {
	const marker = "marker-4711" // in a value of every command
	agent, err := model.NewAgent("rolled-back", "name "+marker, "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	upsert := &raftv1.Op{Op: &raftv1.Op_AgentUpsert{AgentUpsert: wire.AgentToProto(agent)}}
	missing := &raftv1.Op{Op: &raftv1.Op_AgentDelete{AgentDelete: "missing-" + marker}}
	notify := &raftv1.Op{Op: &raftv1.Op_EventNotify{EventNotify: marker}}
	command := func(ops ...*raftv1.Op) []byte {
		data, err := overlay.EncodeCommand(ops)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	for _, tc := range []struct {
		name string
		data []byte
		kind string // of the failed command; empty when it applies
		ops  int    // of one that decodes
	}{
		{"applies", command(upsert), "", 0},
		{"only events", command(notify), "", 0},
		{"does not decode", []byte("not a command " + marker), undecodable, 0},
		{"has an operation of no known kind", command(&raftv1.Op{}), undecodable, 0},
		{"fails on its only operation", command(missing), "agent_delete", 1},
		{"fails on its second operation", command(upsert, missing), "agent_upsert,agent_delete", 2},
		{"fails with events", command(notify, missing), "event_notify,agent_delete", 2},
		{"names each kind once", command(missing, missing, missing), "agent_delete", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logBuffer{}
			store := inmemory.New()
			f := NewFSM(store, nil, zerolog.New(logs))
			got := f.Apply(&hraft.Log{Index: 42, Data: tc.data})

			lines := logLines(t, logs)
			if tc.kind == "" {
				if got != nil || len(lines) != 0 {
					t.Fatalf("Apply = %v and logged %v, want nil and nothing", got, lines)
				}
				return
			}
			if err, _ := got.(error); err == nil {
				t.Fatalf("Apply = %v, want the error of the command", got)
			}
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want 1: %s", len(lines), logs)
			}
			line := lines[0]
			if ops, logged := line["ops"].(float64); line["level"] != "error" || line["index"] != 42.0 || line["kind"] != tc.kind || logged != (tc.ops != 0) || int(ops) != tc.ops {
				t.Fatalf("logged %v, want an ERROR of kind %q at index 42 with %d operations", line, tc.kind, tc.ops)
			}
			for key := range line {
				switch key {
				case "level", "message", "index", "kind", "ops":
				default:
					t.Fatalf("the line has the field %q: %v", key, line)
				}
			}
			if strings.Contains(logs.String(), marker) {
				t.Fatalf("the log holds a value of the command: %s", logs)
			}
			if _, err := store.GetAgent(context.Background(), agent.ID()); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("a command that failed left its first operation applied: %v", err)
			}
		})
	}
}
