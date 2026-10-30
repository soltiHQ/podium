package overlay

import (
	"bytes"
	"fmt"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// CommandMagic starts every SOLTI-CMD command, before its version byte.
	CommandMagic = "SOLTI-CMD"
	// CommandVersion is the version EncodeCommand writes. DecodeCommand also reads
	// version 2, which has no execution archive operations.
	CommandVersion byte = 3
)

// EncodeCommand frames ops as a SOLTI-CMD command: a format guard before the
// authoritative protobuf payload. The header prevents legacy Agent capability
// strings from being decoded as the current structured message during log
// recovery or mixed-version traffic.
func EncodeCommand(ops []*raftv1.Op) ([]byte, error) {
	payload, err := proto.Marshal(&raftv1.Command{Ops: ops})
	if err != nil {
		return nil, fmt.Errorf("encode command: %w", err)
	}
	data := make([]byte, len(CommandMagic)+1+len(payload))
	copy(data, CommandMagic)
	data[len(CommandMagic)] = CommandVersion
	copy(data[len(CommandMagic)+1:], payload)
	return data, nil
}

// DecodeCommand returns the command EncodeCommand framed, or one of version 2. It
// refuses a headerless legacy command, another version, an unknown or missing
// operation, unknown fields, and an archive operation in a version 2 command.
func DecodeCommand(data []byte) (*raftv1.Command, error) {
	if len(data) < len(CommandMagic)+1 || !bytes.HasPrefix(data, []byte(CommandMagic)) {
		return nil, fmt.Errorf("unsupported legacy command format; back up the data directory and migrate offline with a compatible Podium version")
	}
	if version := data[len(CommandMagic)]; version != CommandVersion && version != 2 {
		return nil, fmt.Errorf("unsupported command version %d (want %d); use a compatible Podium version or migrate the backed-up data directory offline", version, CommandVersion)
	}
	var cmd raftv1.Command
	if err := proto.Unmarshal(data[len(CommandMagic)+1:], &cmd); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	for _, op := range cmd.GetOps() {
		if op == nil || op.GetOp() == nil || len(op.ProtoReflect().GetUnknown()) != 0 {
			return nil, fmt.Errorf("unknown or missing command operation; use a compatible Podium version")
		}
		if data[len(CommandMagic)] == 2 {
			switch op.GetOp().(type) {
			case *raftv1.Op_ExecutionUpsert, *raftv1.Op_ExecutionDelete:
				return nil, fmt.Errorf("execution archive operation requires command version 3")
			}
		}
	}
	if len(cmd.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("unknown command fields; use a compatible Podium version")
	}
	return &cmd, nil
}
