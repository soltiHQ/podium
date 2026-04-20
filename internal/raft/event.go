package raft

import (
	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/event"
)

// Compile-time check.
var _ event.Writer = (*EventWriter)(nil)

// EventWriter replicates hub mutations through Raft. Plugged into
// [event.Hub] via hub.SetWriter so Notify/Record/DeleteIssues on any
// replica submit a [Command] and the FSM fires on every replica via
// ApplyLocal*.
//
// Non-leader submissions fall back to local-only execution: Raft rejects
// them with ErrNotLeader and the follower's own hub still needs to reflect
// the event. In a typical flow writes already hit the leader (via leader
// forwarding in transport middleware), so this path is rare.
type EventWriter struct {
	raft *hraft.Raft
	hub  *event.Hub // local hub for follower fallback
}

// NewEventWriter wires the Raft node to the local hub. hub is the same
// *event.Hub the FSM applies against.
func NewEventWriter(r *hraft.Raft, hub *event.Hub) *EventWriter {
	return &EventWriter{raft: r, hub: hub}
}

func (w *EventWriter) Notify(ev string) {
	if err := w.submit(Op{Code: OpEventNotify, EventName: ev}); err != nil {
		// Submit failed — fall back to local-only emission so the caller's
		// SSE subscribers at least see it on this replica.
		w.hub.ApplyLocalNotify(ev)
	}
}

func (w *EventWriter) Record(kind string, p event.Payload) {
	if err := w.submit(Op{Code: OpEventRecord, EventKind: kind, EventPayload: p}); err != nil {
		w.hub.ApplyLocalRecord(kind, p)
	}
}

func (w *EventWriter) DeleteIssues(kind, id string) int {
	// Count returned by Raft-backed path is not meaningful across replicas
	// (count is per-replica ring). Return the local count after the
	// replicated apply — same contract as standalone.
	_ = w.submit(Op{Code: OpEventDeleteIssues, EventKind: kind, ID: id})
	return w.hub.ApplyLocalDeleteIssues(kind, id)
}

// submit encodes and applies a single-op command. Returns error if Raft
// rejects the submission (non-leader, timeout, quorum loss).
func (w *EventWriter) submit(op Op) error {
	data, err := Command{Ops: []Op{op}}.Encode()
	if err != nil {
		return err
	}
	return w.raft.Apply(data, applyTimeout).Error()
}
