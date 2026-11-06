package overlay_test

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/storage/overlay"
)

// withVersion is a copy of a command with its version byte set to version.
func withVersion(command []byte, version byte) []byte {
	out := append([]byte(nil), command...)
	out[len(overlay.CommandMagic)] = version
	return out
}

// Commands are written in version 4, which carries derived agent credentials,
// and read in versions 3 and 4. A version-3 command, which a previous version
// logged, decodes as written, but one that carries a derived credential, which
// no version-3 writer made, is refused. Versions 2, 1 and 5 are refused with
// the version error.
func TestCommandVersions(t *testing.T) {
	generation := uint32(1)
	derived := &raftv1.Op{Op: &raftv1.Op_AgentCredentialUpsert{AgentCredentialUpsert: &raftv1.AgentCredentialMsg{
		AgentId: "agent-1", Generation: &generation, SecretId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, CreatedAtNs: 1, UpdatedAtNs: 1,
	}}}
	stored := &raftv1.Op{Op: &raftv1.Op_AgentCredentialUpsert{AgentCredentialUpsert: &raftv1.AgentCredentialMsg{
		AgentId: "agent-2", Token: "raw-token", CreatedAtNs: 1, UpdatedAtNs: 1,
	}}}
	withDerived, err := overlay.EncodeCommand([]*raftv1.Op{stored, derived})
	if err != nil {
		t.Fatal(err)
	}
	if v := withDerived[len(overlay.CommandMagic)]; v != 4 || overlay.CommandVersion != 4 {
		t.Fatalf("a new command is version %d (CommandVersion %d), want 4", v, overlay.CommandVersion)
	}
	decoded, err := overlay.DecodeCommand(withDerived)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decoded, &raftv1.Command{Ops: []*raftv1.Op{stored, derived}}) {
		t.Fatal("a version-4 command changed on its round trip")
	}

	storedOnly, err := overlay.EncodeCommand([]*raftv1.Op{stored, {Op: &raftv1.Op_AgentDelete{AgentDelete: "agent-3"}}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = overlay.DecodeCommand(withVersion(storedOnly, 3))
	if err != nil {
		t.Fatalf("a version-3 command: %v", err)
	}
	if len(decoded.GetOps()) != 2 || decoded.GetOps()[0].GetAgentCredentialUpsert().GetToken() != "raw-token" {
		t.Fatal("a version-3 command did not decode as written")
	}

	for _, tc := range []struct {
		name    string
		command []byte
		want    string
	}{
		{"version 3 with a derived credential", withVersion(withDerived, 3), "derived agent credential requires command version 4"},
		{"version 3 with a secret ID", withVersion(mustEncode(t, &raftv1.Op{Op: &raftv1.Op_AgentCredentialUpsert{AgentCredentialUpsert: &raftv1.AgentCredentialMsg{
			AgentId: "agent-2", Token: "raw-token", SecretId: []byte{1}, CreatedAtNs: 1,
		}}}), 3), "derived agent credential requires command version 4"},
		{"version 2", withVersion(storedOnly, 2), "unsupported command version 2"},
		{"version 1", withVersion(storedOnly, 1), "unsupported command version 1"},
		{"version 5", withVersion(storedOnly, 5), "unsupported command version 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := overlay.DecodeCommand(tc.command); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DecodeCommand: %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func mustEncode(t *testing.T, ops ...*raftv1.Op) []byte {
	t.Helper()
	data, err := overlay.EncodeCommand(ops)
	if err != nil {
		t.Fatal(fmt.Errorf("encode: %w", err))
	}
	return data
}
