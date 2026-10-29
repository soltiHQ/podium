// Package snapshot owns the versioned control-plane state codec shared by
// Raft compaction and standalone durable storage. A failed decode never changes
// the live store; callers publish the returned state only after success.
package snapshot

import (
	"fmt"
	"sort"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	Magic   = "SOLTI-SNAP"
	Version = uint32(3)
)

// Marshal encodes a complete snapshot with deterministic entity/map ordering.
func Marshal(content inmemory.SnapshotContent) ([]byte, error) {
	if err := validate(content); err != nil {
		return nil, err
	}
	content.Agents = sortedEntities(content.Agents)
	content.AgentCreds = sortedEntities(content.AgentCreds)
	content.Users = sortedEntities(content.Users)
	content.Roles = sortedEntities(content.Roles)
	content.Credentials = sortedEntities(content.Credentials)
	content.Verifiers = sortedEntities(content.Verifiers)
	content.Sessions = sortedEntities(content.Sessions)
	content.Specs = sortedEntities(content.Specs)
	content.Rollouts = sortedEntities(content.Rollouts)
	content.Executions = sortedEntities(content.Executions)
	msg := &raftv1.Snapshot{
		Header: &raftv1.SnapshotHeader{
			Magic:   Magic,
			Version: Version,
		},
		Agents:           make([]*raftv1.AgentMsg, 0, len(content.Agents)),
		AgentCredentials: make([]*raftv1.AgentCredentialMsg, 0, len(content.AgentCreds)),
		Users:            make([]*raftv1.UserMsg, 0, len(content.Users)),
		Roles:            make([]*raftv1.RoleMsg, 0, len(content.Roles)),
		Credentials:      make([]*raftv1.CredentialMsg, 0, len(content.Credentials)),
		Verifiers:        make([]*raftv1.VerifierMsg, 0, len(content.Verifiers)),
		Sessions:         make([]*raftv1.SessionMsg, 0, len(content.Sessions)),
		Specs:            make([]*raftv1.SpecMsg, 0, len(content.Specs)),
		Rollouts:         make([]*raftv1.RolloutMsg, 0, len(content.Rollouts)),
		Executions:       make([]*raftv1.ExecutionRecordMsg, 0, len(content.Executions)),
	}
	for _, a := range content.Agents {
		msg.Agents = append(msg.Agents, wire.AgentToProto(a))
	}
	for _, c := range content.AgentCreds {
		msg.AgentCredentials = append(msg.AgentCredentials, wire.AgentCredentialToProto(c))
	}
	for _, u := range content.Users {
		msg.Users = append(msg.Users, wire.UserToProto(u))
	}
	for _, r := range content.Roles {
		msg.Roles = append(msg.Roles, wire.RoleToProto(r))
	}
	for _, c := range content.Credentials {
		msg.Credentials = append(msg.Credentials, wire.CredentialToProto(c))
	}
	for _, v := range content.Verifiers {
		msg.Verifiers = append(msg.Verifiers, wire.VerifierToProto(v))
	}
	for _, ss := range content.Sessions {
		msg.Sessions = append(msg.Sessions, wire.SessionToProto(ss))
	}
	for _, sp := range content.Specs {
		encoded, err := wire.SpecToProto(sp)
		if err != nil {
			return nil, fmt.Errorf("storage snapshot: spec %q: %w", sp.ID(), err)
		}
		msg.Specs = append(msg.Specs, encoded)
	}
	for _, ro := range content.Rollouts {
		msg.Rollouts = append(msg.Rollouts, wire.RolloutToProto(ro))
	}

	for _, e := range content.Executions {
		msg.Executions = append(msg.Executions, wire.ExecutionToProto(e))
	}

	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("storage snapshot: marshal: %w", err)
	}
	return data, nil
}

// Unmarshal validates the header and every entity before returning any state.
// Duplicate IDs are rejected rather than silently replacing an earlier record.
func Unmarshal(data []byte) (inmemory.SnapshotContent, error) {
	var content inmemory.SnapshotContent
	if err := validateHeader(data); err != nil {
		return content, err
	}
	var snap raftv1.Snapshot
	if err := proto.Unmarshal(data, &snap); err != nil {
		return content, fmt.Errorf("storage snapshot: unmarshal: %w", err)
	}

	hdr := snap.GetHeader()
	if hdr.GetMagic() != Magic {
		return content, fmt.Errorf("storage snapshot: bad magic %q (want %q)", hdr.GetMagic(), Magic)
	}
	if hdr.GetVersion() != Version && hdr.GetVersion() != 2 {
		return content, fmt.Errorf("storage snapshot: unsupported version %d (want %d)", hdr.GetVersion(), Version)
	}

	for _, m := range snap.GetAgents() {
		a, err := wire.AgentFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: agent: %w", err)
		}
		content.Agents = append(content.Agents, a)
	}
	for _, m := range snap.GetAgentCredentials() {
		c, err := wire.AgentCredentialFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: agent credential: %w", err)
		}
		content.AgentCreds = append(content.AgentCreds, c)
	}
	for _, m := range snap.GetUsers() {
		u, err := wire.UserFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: user: %w", err)
		}
		content.Users = append(content.Users, u)
	}
	for _, m := range snap.GetRoles() {
		ro, err := wire.RoleFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: role: %w", err)
		}
		content.Roles = append(content.Roles, ro)
	}
	for _, m := range snap.GetCredentials() {
		c, err := wire.CredentialFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: credential: %w", err)
		}
		content.Credentials = append(content.Credentials, c)
	}
	for _, m := range snap.GetVerifiers() {
		ver, err := wire.VerifierFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: verifier: %w", err)
		}
		content.Verifiers = append(content.Verifiers, ver)
	}
	for _, m := range snap.GetSessions() {
		s, err := wire.SessionFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: session: %w", err)
		}
		content.Sessions = append(content.Sessions, s)
	}
	for _, m := range snap.GetSpecs() {
		sp, err := wire.SpecFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: spec: %w", err)
		}
		content.Specs = append(content.Specs, sp)
	}
	for _, m := range snap.GetRollouts() {
		ro, err := wire.RolloutFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: rollout: %w", err)
		}
		content.Rollouts = append(content.Rollouts, ro)
	}

	if hdr.GetVersion() == 2 && len(snap.GetExecutions()) != 0 {
		return content, fmt.Errorf("storage snapshot: version 2 cannot contain execution records")
	}
	for _, m := range snap.GetExecutions() {
		e, err := wire.ExecutionFromProto(m)
		if err != nil {
			return content, fmt.Errorf("storage snapshot: execution: %w", err)
		}
		content.Executions = append(content.Executions, e)
	}

	if err := validate(content); err != nil {
		return content, err
	}
	return content, nil
}

func validate(c inmemory.SnapshotContent) error {
	if err := validateEntities(c.Agents); err != nil {
		return fmt.Errorf("storage snapshot: agents: %w", err)
	}
	if err := validateEntities(c.AgentCreds); err != nil {
		return fmt.Errorf("storage snapshot: agentcreds: %w", err)
	}
	if err := validateEntities(c.Users); err != nil {
		return fmt.Errorf("storage snapshot: users: %w", err)
	}
	if err := validateEntities(c.Roles); err != nil {
		return fmt.Errorf("storage snapshot: roles: %w", err)
	}
	if err := validateEntities(c.Credentials); err != nil {
		return fmt.Errorf("storage snapshot: credentials: %w", err)
	}
	if err := validateEntities(c.Verifiers); err != nil {
		return fmt.Errorf("storage snapshot: verifiers: %w", err)
	}
	if err := validateEntities(c.Sessions); err != nil {
		return fmt.Errorf("storage snapshot: sessions: %w", err)
	}
	if err := validateEntities(c.Specs); err != nil {
		return fmt.Errorf("storage snapshot: specs: %w", err)
	}
	if err := validateEntities(c.Rollouts); err != nil {
		return fmt.Errorf("storage snapshot: rollouts: %w", err)
	}

	if err := validateEntities(c.Executions); err != nil {
		return fmt.Errorf("storage snapshot: executions: %w", err)
	}
	for _, e := range c.Executions {
		next, err := model.NewExecutionRecord(e.Params())
		if err != nil || next.ID() != e.ID() {
			return fmt.Errorf("storage snapshot: invalid execution record")
		}
	}
	return nil
}

func validateEntities[T domain.Entity[T]](items []T) error {
	seen := make(map[string]struct{}, len(items))
	var zero T
	for i, item := range items {
		if any(item) == any(zero) {
			return fmt.Errorf("nil entity at index %d", i)
		}
		id := item.ID()
		if id == "" || item.CreatedAt().IsZero() {
			return fmt.Errorf("invalid entity at index %d", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func sortedEntities[T domain.Entity[T]](items []T) []T {
	result := append([]T(nil), items...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID() < result[j].ID() })
	return result
}

// Validate the envelope header before parsing entity messages: a legacy agent
// capabilities payload uses the same field number with an incompatible schema.
// Skipping entity bytes lets us report version rejection before attempting to
// interpret that payload. Header duplication is rejected as malformed state.
func validateHeader(data []byte) error {
	found := false
	for len(data) > 0 {
		field, kind, n := protowire.ConsumeTag(data)
		if n < 0 {
			return fmt.Errorf("storage snapshot: envelope: %w", protowire.ParseError(n))
		}
		data = data[n:]
		if field == 1 {
			if kind != protowire.BytesType || found {
				return fmt.Errorf("storage snapshot: invalid or duplicate header")
			}
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return fmt.Errorf("storage snapshot: header: %w", protowire.ParseError(n))
			}
			var header raftv1.SnapshotHeader
			if err := proto.Unmarshal(value, &header); err != nil {
				return fmt.Errorf("storage snapshot: header: %w", err)
			}
			if header.GetMagic() != Magic {
				return fmt.Errorf("storage snapshot: bad magic %q (want %q)", header.GetMagic(), Magic)
			}
			if header.GetVersion() != Version && header.GetVersion() != 2 {
				return fmt.Errorf("storage snapshot: unsupported version %d (want %d); restore a compatible backup or migrate offline", header.GetVersion(), Version)
			}
			found = true
			data = data[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(field, kind, data)
		if n < 0 {
			return fmt.Errorf("storage snapshot: envelope: %w", protowire.ParseError(n))
		}
		data = data[n:]
	}
	if !found {
		return fmt.Errorf("storage snapshot: missing header")
	}
	return nil
}
