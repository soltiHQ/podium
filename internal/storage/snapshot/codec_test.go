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
