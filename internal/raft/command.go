package raft

import (
	"bytes"
	"encoding/gob"
	"fmt"

	"github.com/soltiHQ/control-plane/domain/wire"
)

// OpCode identifies the mutation carried by an [Op].
type OpCode uint8

const (
	OpUnknown OpCode = iota

	OpAgentUpsert
	OpAgentDelete

	OpUserUpsert
	OpUserDelete

	OpRoleUpsert
	OpRoleDelete

	OpCredentialUpsert
	OpCredentialDelete

	OpVerifierUpsert
	OpVerifierDelete
	OpVerifierDeleteByCredential

	OpSessionCreate
	OpSessionDelete
	OpSessionDeleteByUser
	OpSessionRotateRefresh
	OpSessionRevoke

	OpSpecUpsert
	OpSpecDelete

	OpRolloutUpsert
	OpRolloutDelete
	OpRolloutDeleteBySpec
)

// Op is a single mutation to apply as part of a [Command].
//
// Only the field matching Code is meaningful; the rest are zero. Encoded
// with gob, so any non-exported fields must be registered via wire.Register.
type Op struct {
	Code OpCode

	AgentUpsert      *wire.AgentDTO
	UserUpsert       *wire.UserDTO
	RoleUpsert       *wire.RoleDTO
	CredentialUpsert *wire.CredentialDTO
	VerifierUpsert   *wire.VerifierDTO
	SessionCreate    *wire.SessionDTO
	SpecUpsert       *wire.SpecDTO
	RolloutUpsert    *wire.RolloutDTO

	// ID fields. Which one is populated depends on Code (matches the
	// Delete / DeleteBy* variants).
	ID     string
	UserID string

	// SessionRotateRefresh params.
	RefreshHash []byte
	ExpiresAtNs int64

	// SessionRevoke param.
	RevokedAtNs int64
}

// Command is the unit of log entry submitted to Raft. One command maps to
// one atomic transaction applied by the FSM.
type Command struct {
	Ops []Op
}

// Encode serialises with gob so custom DTO types roundtrip faithfully.
func (c Command) Encode() ([]byte, error) {
	wire.Register()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(c); err != nil {
		return nil, fmt.Errorf("raft: encode command: %w", err)
	}
	return buf.Bytes(), nil
}

// DecodeCommand inverts Encode.
func DecodeCommand(data []byte) (Command, error) {
	wire.Register()
	var c Command
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&c); err != nil {
		return Command{}, fmt.Errorf("raft: decode command: %w", err)
	}
	return c, nil
}
