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
	// CommandVersion is the version EncodeCommand writes, which carries derived
	// agent credentials. DecodeCommand also reads version 3, which has none.
	CommandVersion byte = 4
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

// DecodeCommand returns the command EncodeCommand framed, or one of version 3. It
// refuses a headerless legacy command, another version, an unknown or missing
// operation, unknown fields, and a derived agent credential in a version 3
// command.
func DecodeCommand(data []byte) (*raftv1.Command, error) {
	if len(data) < len(CommandMagic)+1 || !bytes.HasPrefix(data, []byte(CommandMagic)) {
		return nil, fmt.Errorf("unsupported legacy command format; back up the data directory and migrate offline with a compatible Podium version")
	}
	version := data[len(CommandMagic)]
	if version != CommandVersion && version != 3 {
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
		if c := op.GetAgentCredentialUpsert(); version == 3 && c != nil && (c.Generation != nil || len(c.GetSecretId()) != 0) {
			return nil, fmt.Errorf("derived agent credential requires command version 4")
		}
	}
	if len(cmd.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("unknown command fields; use a compatible Podium version")
	}
	return &cmd, nil
}
