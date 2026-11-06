package wire_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
)

// nowTruncated returns time.Now() rounded to nanosecond — the proto wire
// format carries Unix nanos, so any sub-nano precision (none on amd64,
// some on arm64) is lost. Round-trip equality needs identical input.
func nowTruncated() time.Time {
	return time.Unix(0, time.Now().UnixNano())
}

func TestAgent_ProtoRoundtrip(t *testing.T) {
	a, err := model.NewAgent("a1", "agent", "http://a:1")
	if err != nil {
		t.Fatal(err)
	}
	a.SetEndpointType(enum.EndpointHTTP)
	a.SetAPIVersion(enum.APIVersion(1))
	a.SetOS("linux")
	a.SetArch("amd64")
	a.SetStatus(enum.AgentStatusActive)
	a.LabelAdd("env", "prod")
	a.SetCreatedAt(nowTruncated())
	a.SetUpdatedAt(a.CreatedAt())
	a.SetLastSeenAt(a.CreatedAt())

	data, err := proto.Marshal(wire.AgentToProto(a))
	if err != nil {
		t.Fatal(err)
	}
	var msg raftv1.AgentMsg
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	back, err := wire.AgentFromProto(&msg)
	if err != nil {
		t.Fatal(err)
	}

	if back.ID() != a.ID() || back.Name() != a.Name() || back.Endpoint() != a.Endpoint() {
		t.Fatalf("id/name/endpoint mismatch: %+v", back)
	}
	if back.OS() != "linux" || back.Arch() != "amd64" {
		t.Fatalf("os/arch: %s / %s", back.OS(), back.Arch())
	}
	if !back.CreatedAt().Equal(a.CreatedAt()) {
		t.Fatalf("createdAt: want %v got %v", a.CreatedAt(), back.CreatedAt())
	}
	if v, _ := back.Label("env"); v != "prod" {
		t.Fatalf("label: got %q", v)
	}
}

func TestUser_ProtoRoundtrip(t *testing.T) {
	u, _ := model.NewUser("u1", "subject@x")
	u.SetEmail("a@b.c")
	u.SetName("Alice")
	_ = u.RoleAdd("role-a")
	_ = u.PermissionAdd(enum.Permission("specs:get"))
	u.SetCreatedAt(nowTruncated())

	back, err := wire.UserFromProto(wire.UserToProto(u))
	if err != nil {
		t.Fatal(err)
	}
	if back.Email() != u.Email() || back.Name() != u.Name() {
		t.Fatal("email/name mismatch")
	}
	if !back.RoleHas("role-a") {
		t.Fatal("role lost")
	}
	if !back.PermissionHas(enum.Permission("specs:get")) {
		t.Fatal("perm lost")
	}
}

func TestSpec_ProtoRoundtrip(t *testing.T) {
	ts, _ := model.NewSpec("s1", "my-spec", "slot-a")
	ts.SetKindType(enum.TaskKindType("subprocess"))
	ts.SetKindConfig(map[string]any{"cmd": "echo"})
	ts.SetTimeoutMs(1000)
	ts.SetTargets([]string{"a1", "a2"})
	ts.SetVersion(3)
	ts.SetGeneration(2)
	ts.MarkForDeletion()
	ts.SetCreatedAt(nowTruncated())

	message, err := wire.SpecToProto(ts)
	if err != nil {
		t.Fatal(err)
	}
	back, err := wire.SpecFromProto(message)
	if err != nil {
		t.Fatal(err)
	}
	if back.Version() != 3 || back.Generation() != 2 {
		t.Fatalf("version/gen: %d/%d", back.Version(), back.Generation())
	}
	if !back.DeletionRequested() {
		t.Fatal("DeletionRequested lost")
	}
	if len(back.Targets()) != 2 {
		t.Fatalf("targets: %v", back.Targets())
	}
}

// TestSpec_KindConfigNestedRoundtrip — typical POST /api/v1/specs comes
// with KindConfig of shape {"env": {...}, "args": [...]} — after JSON
// unmarshal it carries map[string]any / []any. Verify structpb passes it
// through proto Marshal+Unmarshal without losing structure.
func TestSpec_KindConfigNestedRoundtrip(t *testing.T) {
	ts, _ := model.NewSpec("s1", "my-spec", "slot")
	ts.SetKindConfig(map[string]any{
		"cmd":    "echo",
		"args":   []any{"hello", "world"},
		"env":    map[string]any{"KEY": "value", "PORT": 8080},
		"config": map[string]any{"nested": map[string]any{"deep": true}},
	})
	ts.SetCreatedAt(nowTruncated())

	message, err := wire.SpecToProto(ts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var msg raftv1.SpecMsg
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back, err := wire.SpecFromProto(&msg)
	if err != nil {
		t.Fatalf("FromProto: %v", err)
	}

	kc := back.KindConfig()
	if kc["cmd"] != "echo" {
		t.Fatalf("cmd lost: %v", kc["cmd"])
	}
	env, ok := kc["env"].(map[string]any)
	if !ok || env["KEY"] != "value" {
		t.Fatalf("nested env lost: %v", kc["env"])
	}
	// structpb stores all numbers as float64.
	if port, _ := env["PORT"].(float64); port != 8080 {
		t.Fatalf("nested numeric lost: %v (type %T)", env["PORT"], env["PORT"])
	}
}

func TestRollout_ProtoRoundtrip(t *testing.T) {
	r, _ := model.NewRollout("s1", "a1", 5)
	r.SetIntent(enum.RolloutIntentUpdate)
	r.SetActualTaskID("task-xyz")
	r.MarkSynced(5)

	back, err := wire.RolloutFromProto(wire.RolloutToProto(r))
	if err != nil {
		t.Fatal(err)
	}
	if back.ActualTaskID() != "task-xyz" {
		t.Fatalf("task id: %s", back.ActualTaskID())
	}
	if back.Status() != enum.SyncStatusSynced {
		t.Fatalf("status: %v", back.Status())
	}
	if back.ObservedGeneration() != 5 {
		t.Fatalf("observed: %d", back.ObservedGeneration())
	}
}

// A stored credential decodes whatever the length of its token: the 1024-byte
// cap applies when discovery enrolls a token, never when a replica replays or
// restores one an older version stored.
func TestAgentCredentialFromProtoKeepsLongTokens(t *testing.T) {
	token := strings.Repeat("t", 4096)
	created := nowTruncated()
	c, err := wire.AgentCredentialFromProto(&raftv1.AgentCredentialMsg{
		AgentId: "a1", Token: token, CreatedAtNs: created.UnixNano(), UpdatedAtNs: created.UnixNano(),
	})
	if err != nil {
		t.Fatalf("decode a 4096-byte token: %v", err)
	}
	if c.AgentID() != "a1" || c.Token() != token || !c.CreatedAt().Equal(created) {
		t.Fatalf("credential %q, %d-byte token, created %v; want a1, the 4096-byte token, %v", c.AgentID(), len(c.Token()), c.CreatedAt(), created)
	}
}

// A derived credential travels as its generation and secret ID, without a
// token, and a stored one as its token, without either: each comes back as the
// kind it was, with every field.
func TestAgentCredential_ProtoRoundtrip(t *testing.T) {
	created := nowTruncated()
	secretID := []byte{0x26, 0xd9, 0x07, 0x67, 0xda, 0x09, 0x1e, 0xd4}
	derived, err := model.NewDerivedAgentCredential("a1", 2, secretID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := model.NewStoredAgentCredential("a2", "raw-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*model.AgentCredential{derived, stored} {
		c.SetCreatedAt(created)
		c.SetUpdatedAt(created.Add(time.Second))
		msg := wire.AgentCredentialToProto(c)
		switch {
		case c.Derived() && (msg.Generation == nil || msg.GetGeneration() != 2 || !bytes.Equal(msg.GetSecretId(), secretID) || msg.GetToken() != ""):
			t.Fatalf("derived credential on the wire: generation %v, secret ID %x, %d-byte token; want 2, %x and no token", msg.Generation, msg.GetSecretId(), len(msg.GetToken()), secretID)
		case !c.Derived() && (msg.Generation != nil || len(msg.GetSecretId()) != 0 || msg.GetToken() != "raw-token"):
			t.Fatalf("stored credential on the wire: generation %v, secret ID %x, token %q; want neither and the token", msg.Generation, msg.GetSecretId(), msg.GetToken())
		}
		back, err := wire.AgentCredentialFromProto(msg)
		if err != nil {
			t.Fatal(err)
		}
		if back.AgentID() != c.AgentID() || back.Derived() != c.Derived() || back.Generation() != c.Generation() ||
			!bytes.Equal(back.SecretID(), c.SecretID()) || back.Token() != c.Token() ||
			!back.CreatedAt().Equal(c.CreatedAt()) || !back.UpdatedAt().Equal(c.UpdatedAt()) {
			t.Fatalf("credential %s changed on the round trip", c.AgentID())
		}
	}
}

// A message that is neither kind, or both, is refused: a generation with a token
// or without a secret ID, a secret ID without a generation, and neither a
// generation nor a token.
func TestAgentCredentialFromProtoRefusesMixedKinds(t *testing.T) {
	zero := uint32(0)
	for _, tc := range []struct {
		name string
		msg  *raftv1.AgentCredentialMsg
	}{
		{"generation and token", &raftv1.AgentCredentialMsg{AgentId: "a1", Token: "raw-token", Generation: &zero, SecretId: []byte{1}, CreatedAtNs: 1}},
		{"generation without a secret ID", &raftv1.AgentCredentialMsg{AgentId: "a1", Generation: &zero, CreatedAtNs: 1}},
		{"secret ID without a generation", &raftv1.AgentCredentialMsg{AgentId: "a1", Token: "raw-token", SecretId: []byte{1}, CreatedAtNs: 1}},
		{"neither generation nor token", &raftv1.AgentCredentialMsg{AgentId: "a1", CreatedAtNs: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := wire.AgentCredentialFromProto(tc.msg); err == nil {
				t.Fatalf("decoded to a credential, derived %v, want an error", c.Derived())
			}
		})
	}
}

// TestCommand_ProtoRoundtrip — a multi-op Command marshals + unmarshals
// without losing op semantics. Belt-and-braces check on the oneof wiring.
func TestCommand_ProtoRoundtrip(t *testing.T) {
	a, _ := model.NewAgent("a1", "agent", "http://a")
	cmd := &raftv1.Command{
		Ops: []*raftv1.Op{
			{Op: &raftv1.Op_AgentUpsert{AgentUpsert: wire.AgentToProto(a)}},
			{Op: &raftv1.Op_AgentDelete{AgentDelete: "a2"}},
			{Op: &raftv1.Op_EventNotify{EventNotify: "agent_update"}},
		},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var back raftv1.Command
	if err := proto.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got := len(back.GetOps()); got != 3 {
		t.Fatalf("ops len: %d", got)
	}
	if u := back.GetOps()[0].GetAgentUpsert(); u == nil || u.GetId() != "a1" {
		t.Fatalf("op[0]: %+v", u)
	}
	if d := back.GetOps()[1].GetAgentDelete(); d != "a2" {
		t.Fatalf("op[1]: %q", d)
	}
	if n := back.GetOps()[2].GetEventNotify(); n != "agent_update" {
		t.Fatalf("op[2]: %q", n)
	}
	// Compare encoded bytes — proto.Equal is the right tool but bytes
	// suffice for "round-trip preserves content".
	again, _ := proto.Marshal(&back)
	if !bytes.Equal(data, again) {
		t.Fatal("re-encoded bytes differ from original")
	}
}
