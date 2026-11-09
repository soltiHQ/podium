package snapshot_test

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
	"google.golang.org/protobuf/proto"
)

func TestDeterministicEncodingAndValidation(t *testing.T) {
	a, _ := model.NewAgent("a1", "first", "http://first")
	b, _ := model.NewAgent("a2", "second", "http://second")
	a.LabelAdd("z", "last")
	a.LabelAdd("a", "first")
	one, err := snapshot.Marshal(inmemory.SnapshotContent{Agents: []*model.Agent{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := snapshot.Marshal(inmemory.SnapshotContent{Agents: []*model.Agent{b, a}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatal("encoding depends on entity order")
	}
	decoded, err := snapshot.Unmarshal(one)
	if err != nil || len(decoded.Agents) != 2 {
		t.Fatalf("decode: %v", err)
	}
	for _, items := range [][]*model.Agent{{nil}, {a, a}} {
		if _, err := snapshot.Marshal(inmemory.SnapshotContent{Agents: items}); err == nil {
			t.Fatal("invalid snapshot encoded")
		}
	}
}

func TestUnmarshalRejectsInvalidState(t *testing.T) {
	a, _ := model.NewAgent("a1", "first", "http://first")
	cases := []struct {
		name string
		msg  *raftv1.Snapshot
	}{
		{"missing header", &raftv1.Snapshot{}},
		{"wrong magic", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: "wrong", Version: snapshot.Version}}},
		{"legacy capabilities schema", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: 1}}},
		{"future version", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: snapshot.Version + 1}}},
		{"duplicate entity", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: snapshot.Version}, Agents: []*raftv1.AgentMsg{wire.AgentToProto(a), wire.AgentToProto(a)}}},
		{"invalid entity", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: snapshot.Version}, Agents: []*raftv1.AgentMsg{{Id: "invalid"}}}},
		{"zero creation time", &raftv1.Snapshot{Header: &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: snapshot.Version}, Agents: []*raftv1.AgentMsg{{Id: "a1", Name: "first", Endpoint: "http://first"}}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			data, err := proto.Marshal(test.msg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := snapshot.Unmarshal(data); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
	if _, err := snapshot.Unmarshal([]byte("not protobuf")); err == nil {
		t.Fatal("invalid protobuf accepted")
	}
}

func TestLegacyHeaderRejectedBeforeIncompatibleEntityDecode(t *testing.T) {
	header, err := proto.Marshal(&raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Legacy AgentMsg field18 was repeated string, now AgentCapabilities message.
	agent := protowire.AppendTag(nil, 18, protowire.BytesType)
	agent = protowire.AppendBytes(agent, []byte("echo"))
	data := protowire.AppendTag(nil, 1, protowire.BytesType)
	data = protowire.AppendBytes(data, header)
	data = protowire.AppendTag(data, 2, protowire.BytesType)
	data = protowire.AppendBytes(data, agent)
	if _, err := snapshot.Unmarshal(data); err == nil || !strings.Contains(err.Error(), "unsupported version 1") {
		t.Fatalf("legacy schema was parsed before header rejection: %v", err)
	}
}

// Snapshots are written in version 4, which carries derived agent credentials,
// and read in versions 3 and 4. A version-3 snapshot, which a previous version
// wrote, decodes with its stored credentials as they were, but one that holds a
// derived credential, which no version-3 writer made, is refused. Versions 2, 1
// and 5 are refused with the version error.
func TestSnapshotVersions(t *testing.T) {
	derived, err := model.NewDerivedAgentCredential("agent-1", 1, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := model.NewStoredAgentCredential("agent-2", "raw-token")
	if err != nil {
		t.Fatal(err)
	}
	data, err := snapshot.Marshal(inmemory.SnapshotContent{AgentCreds: []*model.AgentCredential{derived, stored}})
	if err != nil {
		t.Fatal(err)
	}
	var msg raftv1.Snapshot
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if v := msg.GetHeader().GetVersion(); v != 4 || snapshot.Version != 4 {
		t.Fatalf("a new snapshot is version %d (Version %d), want 4", v, snapshot.Version)
	}
	content, err := snapshot.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.AgentCreds) != 2 || !content.AgentCreds[0].Derived() || content.AgentCreds[0].Generation() != 1 ||
		content.AgentCreds[1].Derived() || content.AgentCreds[1].Token() != "raw-token" {
		t.Fatal("a version-4 snapshot changed its credentials on the round trip")
	}

	encode := func(change func(*raftv1.Snapshot)) []byte {
		t.Helper()
		m := proto.Clone(&msg).(*raftv1.Snapshot)
		change(m)
		out, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	storedOnly := func(version uint32) func(*raftv1.Snapshot) {
		return func(m *raftv1.Snapshot) {
			m.Header.Version = version
			m.AgentCredentials = m.AgentCredentials[1:]
		}
	}
	content, err = snapshot.Unmarshal(encode(storedOnly(3)))
	if err != nil {
		t.Fatalf("a version-3 snapshot: %v", err)
	}
	if len(content.AgentCreds) != 1 || content.AgentCreds[0].Derived() || content.AgentCreds[0].Token() != "raw-token" {
		t.Fatal("a version-3 snapshot did not decode its stored credential as written")
	}

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"version 3 with a derived credential", encode(func(m *raftv1.Snapshot) { m.Header.Version = 3 }), "version 3 cannot contain derived agent credentials"},
		{"version 2", encode(storedOnly(2)), "unsupported version 2"},
		{"version 1", encode(storedOnly(1)), "unsupported version 1"},
		{"version 5", encode(storedOnly(5)), "unsupported version 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := snapshot.Unmarshal(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Unmarshal: %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
