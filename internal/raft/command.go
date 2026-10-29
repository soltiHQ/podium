package raft

import (
	"bytes"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"google.golang.org/protobuf/proto"
)

const (
	commandMagic        = "SOLTI-CMD"
	commandVersion byte = 3
)

// encodeCommand adds a format guard before the authoritative protobuf payload.
// The header prevents legacy Agent capability strings from being decoded as
// the current structured message during log recovery or mixed-version traffic.
func encodeCommand(ops []*raftv1.Op) ([]byte, error) {
	payload, err := proto.Marshal(&raftv1.Command{Ops: ops})
	if err != nil {
		return nil, fmt.Errorf("raft: encode command: %w", err)
	}
	data := make([]byte, len(commandMagic)+1+len(payload))
	copy(data, commandMagic)
	data[len(commandMagic)] = commandVersion
	copy(data[len(commandMagic)+1:], payload)
	return data, nil
}

func decodeCommand(data []byte) (*raftv1.Command, error) {
	if len(data) < len(commandMagic)+1 || !bytes.HasPrefix(data, []byte(commandMagic)) {
		return nil, fmt.Errorf("raft: unsupported legacy command format; back up the data directory and migrate offline with a compatible Podium version")
	}
	if version := data[len(commandMagic)]; version != commandVersion && version != 2 {
		return nil, fmt.Errorf("raft: unsupported command version %d (want %d); use a compatible Podium version or migrate the backed-up data directory offline", version, commandVersion)
	}
	var cmd raftv1.Command
	if err := proto.Unmarshal(data[len(commandMagic)+1:], &cmd); err != nil {
		return nil, fmt.Errorf("raft: decode command: %w", err)
	}
	for _, op := range cmd.GetOps() {
		if op == nil || op.GetOp() == nil || len(op.ProtoReflect().GetUnknown()) != 0 {
			return nil, fmt.Errorf("raft: unknown or missing command operation; use a compatible Podium version")
		}
		if data[len(commandMagic)] == 2 {
			switch op.GetOp().(type) {
			case *raftv1.Op_ExecutionUpsert, *raftv1.Op_ExecutionDelete:
				return nil, fmt.Errorf("raft: execution archive operation requires command version 3")
			}
		}
	}
	if len(cmd.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("raft: unknown command fields; use a compatible Podium version")
	}
	return &cmd, nil
}
