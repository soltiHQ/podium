package snapshot

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// The per-entity JSON codecs encode one entity as the protojson of the
// snapshot's message for it, the message raft already versions, and decode it
// back. The SQL store keeps each row's body so. protojson writes a NUL in a
// string as the escape \u0000, so a body carries every string of the model that
// is valid UTF-8; a proto3 string field refuses one that is not, as raft's log
// does.
//
// A Marshal function refuses, with storage.ErrInvalidArgument, an entity its
// JSON would not carry exactly: nil; a string that is not valid UTF-8; a spec
// config the message cannot represent; and a time, other than the zero time,
// outside the years 1678 to 2262, as the message holds a time in Unix
// nanoseconds, or equal to the Unix epoch in a message that reads 0 back as the
// zero time, which is every message but the session's: its message says which
// of its times are set.
//
// An Unmarshal function answers storage.ErrInternal for a body that is not the
// JSON of the message, or that decodes to an entity storage.ValidateEntity
// refuses: a store wrote the body, so it is corrupt. A field the message does
// not know, one a newer podium wrote, is ignored. Neither error carries any of
// the entity or the body.
var (
	jsonOut = protojson.MarshalOptions{}
	jsonIn  = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// MarshalAgentJSON encodes a as the JSON of raftv1.AgentMsg.
func MarshalAgentJSON(a *model.Agent) ([]byte, error) {
	if a == nil {
		return nil, refused("agent", "it is nil")
	}
	if err := carried("agent", false, a.CreatedAt(), a.UpdatedAt(), a.LastSeenAt(), a.StaleAt()); err != nil {
		return nil, err
	}
	return encode("agent", wire.AgentToProto(a))
}

// UnmarshalAgentJSON decodes the agent MarshalAgentJSON encoded.
func UnmarshalAgentJSON(b []byte) (*model.Agent, error) {
	var m raftv1.AgentMsg
	if err := decode("agent", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.AgentFromProto(&m)
	return restored("agent", e, err)
}

// MarshalAgentCredentialJSON encodes c as the JSON of raftv1.AgentCredentialMsg.
func MarshalAgentCredentialJSON(c *model.AgentCredential) ([]byte, error) {
	if c == nil {
		return nil, refused("agent credential", "it is nil")
	}
	if err := carried("agent credential", false, c.CreatedAt(), c.UpdatedAt()); err != nil {
		return nil, err
	}
	return encode("agent credential", wire.AgentCredentialToProto(c))
}

// UnmarshalAgentCredentialJSON decodes the agent credential
// MarshalAgentCredentialJSON encoded.
func UnmarshalAgentCredentialJSON(b []byte) (*model.AgentCredential, error) {
	var m raftv1.AgentCredentialMsg
	if err := decode("agent credential", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.AgentCredentialFromProto(&m)
	return restored("agent credential", e, err)
}

// MarshalUserJSON encodes u as the JSON of raftv1.UserMsg.
func MarshalUserJSON(u *model.User) ([]byte, error) {
	if u == nil {
		return nil, refused("user", "it is nil")
	}
	if err := carried("user", false, u.CreatedAt(), u.UpdatedAt()); err != nil {
		return nil, err
	}
	return encode("user", wire.UserToProto(u))
}

// UnmarshalUserJSON decodes the user MarshalUserJSON encoded.
func UnmarshalUserJSON(b []byte) (*model.User, error) {
	var m raftv1.UserMsg
	if err := decode("user", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.UserFromProto(&m)
	return restored("user", e, err)
}

// MarshalCredentialJSON encodes c as the JSON of raftv1.CredentialMsg.
func MarshalCredentialJSON(c *model.Credential) ([]byte, error) {
	if c == nil {
		return nil, refused("credential", "it is nil")
	}
	if err := carried("credential", false, c.CreatedAt(), c.UpdatedAt()); err != nil {
		return nil, err
	}
	return encode("credential", wire.CredentialToProto(c))
}

// UnmarshalCredentialJSON decodes the credential MarshalCredentialJSON encoded.
func UnmarshalCredentialJSON(b []byte) (*model.Credential, error) {
	var m raftv1.CredentialMsg
	if err := decode("credential", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.CredentialFromProto(&m)
	return restored("credential", e, err)
}

// MarshalVerifierJSON encodes v as the JSON of raftv1.VerifierMsg.
func MarshalVerifierJSON(v *model.Verifier) ([]byte, error) {
	if v == nil {
		return nil, refused("verifier", "it is nil")
	}
	if err := carried("verifier", false, v.CreatedAt(), v.UpdatedAt()); err != nil {
		return nil, err
	}
	return encode("verifier", wire.VerifierToProto(v))
}

// UnmarshalVerifierJSON decodes the verifier MarshalVerifierJSON encoded.
func UnmarshalVerifierJSON(b []byte) (*model.Verifier, error) {
	var m raftv1.VerifierMsg
	if err := decode("verifier", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.VerifierFromProto(&m)
	return restored("verifier", e, err)
}

// MarshalSessionJSON encodes s as the JSON of raftv1.SessionMsg. The message
// says which of the session's times are set, so the Unix epoch is one it
// carries.
func MarshalSessionJSON(s *model.Session) ([]byte, error) {
	if s == nil {
		return nil, refused("session", "it is nil")
	}
	if err := carried("session", true, s.CreatedAt(), s.UpdatedAt(), s.ExpiresAt(), s.RevokedAt()); err != nil {
		return nil, err
	}
	return encode("session", wire.SessionToProto(s))
}

// UnmarshalSessionJSON decodes the session MarshalSessionJSON encoded.
func UnmarshalSessionJSON(b []byte) (*model.Session, error) {
	var m raftv1.SessionMsg
	if err := decode("session", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.SessionFromProto(&m)
	return restored("session", e, err)
}

// MarshalRoleJSON encodes r as the JSON of raftv1.RoleMsg.
func MarshalRoleJSON(r *model.Role) ([]byte, error) {
	if r == nil {
		return nil, refused("role", "it is nil")
	}
	if err := carried("role", false, r.CreatedAt(), r.UpdatedAt()); err != nil {
		return nil, err
	}
	return encode("role", wire.RoleToProto(r))
}

// UnmarshalRoleJSON decodes the role MarshalRoleJSON encoded.
func UnmarshalRoleJSON(b []byte) (*model.Role, error) {
	var m raftv1.RoleMsg
	if err := decode("role", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.RoleFromProto(&m)
	return restored("role", e, err)
}

// MarshalSpecJSON encodes s as the JSON of raftv1.SpecMsg.
func MarshalSpecJSON(s *model.Spec) ([]byte, error) {
	if s == nil {
		return nil, refused("spec", "it is nil")
	}
	if err := carried("spec", false, s.CreatedAt(), s.UpdatedAt()); err != nil {
		return nil, err
	}
	m, err := wire.SpecToProto(s)
	if err != nil {
		// As the raft transaction overlay refuses it.
		return nil, fmt.Errorf("%w: snapshot: the spec: %w", storage.ErrInvalidArgument, err)
	}
	return encode("spec", m)
}

// UnmarshalSpecJSON decodes the spec MarshalSpecJSON encoded.
func UnmarshalSpecJSON(b []byte) (*model.Spec, error) {
	var m raftv1.SpecMsg
	if err := decode("spec", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.SpecFromProto(&m)
	return restored("spec", e, err)
}

// MarshalRolloutJSON encodes r as the JSON of raftv1.RolloutMsg.
func MarshalRolloutJSON(r *model.Rollout) ([]byte, error) {
	if r == nil {
		return nil, refused("rollout", "it is nil")
	}
	if err := carried("rollout", false, r.CreatedAt(), r.UpdatedAt(), r.LastPushedAt(), r.LastSyncedAt()); err != nil {
		return nil, err
	}
	return encode("rollout", wire.RolloutToProto(r))
}

// UnmarshalRolloutJSON decodes the rollout MarshalRolloutJSON encoded.
func UnmarshalRolloutJSON(b []byte) (*model.Rollout, error) {
	var m raftv1.RolloutMsg
	if err := decode("rollout", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.RolloutFromProto(&m)
	return restored("rollout", e, err)
}

// MarshalExecutionRecordJSON encodes e as the JSON of raftv1.ExecutionRecordMsg.
// The record's constructor already refuses a capture time the message does not
// carry.
func MarshalExecutionRecordJSON(e *model.ExecutionRecord) ([]byte, error) {
	if e == nil {
		return nil, refused("execution record", "it is nil")
	}
	return encode("execution record", wire.ExecutionToProto(e))
}

// UnmarshalExecutionRecordJSON decodes the execution record
// MarshalExecutionRecordJSON encoded; the record's constructor checks it again,
// its ID against its run's identity included.
func UnmarshalExecutionRecordJSON(b []byte) (*model.ExecutionRecord, error) {
	var m raftv1.ExecutionRecordMsg
	if err := decode("execution record", b, &m); err != nil {
		return nil, err
	}
	e, err := wire.ExecutionFromProto(&m)
	return restored("execution record", e, err)
}

// refused is the error of a Marshal function that cannot encode what, for why.
func refused(what, why string) error {
	return fmt.Errorf("%w: snapshot: cannot encode the %s: %s", storage.ErrInvalidArgument, what, why)
}

// carried refuses the times of what that its message would not carry exactly:
// one outside the Unix nanoseconds of an int64, the years 1678 to 2262, and,
// unless epoch, the Unix epoch, which a message without presence flags reads
// back as the zero time. The zero time is carried, as 0.
func carried(what string, epoch bool, times ...time.Time) error {
	for _, t := range times {
		if t.IsZero() {
			continue
		}
		ns := t.UnixNano()
		switch {
		case !time.Unix(0, ns).Equal(t):
			return refused(what, fmt.Sprintf("its time %s is outside the years 1678 to 2262", t.UTC().Format(time.RFC3339)))
		case ns == 0 && !epoch:
			return refused(what, "its message reads a time at the Unix epoch back as the zero time")
		}
	}
	return nil
}

// encode returns the JSON of m, the message of what.
func encode(what string, m proto.Message) ([]byte, error) {
	b, err := jsonOut.Marshal(m)
	if err != nil {
		// protojson names the field it refuses, a string that is not valid
		// UTF-8, and none of its value.
		return nil, fmt.Errorf("%w: snapshot: cannot encode the %s: %w", storage.ErrInvalidArgument, what, err)
	}
	return b, nil
}

// decode reads b, the JSON of m, the message of what. Its error carries none of
// b: protojson's quotes the body.
func decode(what string, b []byte, m proto.Message) error {
	if err := jsonIn.Unmarshal(b, m); err != nil {
		return fmt.Errorf("%w: snapshot: the %s is not the JSON of its message", storage.ErrInternal, what)
	}
	return nil
}

// restored returns the entity a message decoded to, refusing one that did not
// decode, or that no store keeps. Its error says why in the model's words, which
// carry no secret, and wraps storage.ErrInternal alone.
func restored[T domain.Entity[T]](what string, e T, err error) (T, error) {
	if err == nil && storage.ValidateEntity(e) != nil {
		err = errors.New("it has no ID or no creation time")
	}
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: snapshot: the %s does not decode to an entity a store keeps: %v", storage.ErrInternal, what, err)
	}
	return e, nil
}
