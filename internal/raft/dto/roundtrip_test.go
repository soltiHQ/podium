package dto_test

import (
	"bytes"
	"encoding/gob"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/raft/dto"
)

func TestAgent_Roundtrip(t *testing.T) {
	dto.Register()
	a, err := model.NewAgent("a1", "agent", "http://a:1")
	if err != nil {
		t.Fatal(err)
	}
	a.SetEndpointType(kind.EndpointHTTP)
	a.SetAPIVersion(kind.APIVersion(1))
	a.SetOS("linux")
	a.SetArch("amd64")
	a.SetStatus(kind.AgentStatusActive)
	a.LabelAdd("env", "prod")
	// Truncate to microsecond granularity — gob drops sub-ns precision.
	a.SetCreatedAt(time.Now().Truncate(time.Microsecond))
	a.SetUpdatedAt(a.CreatedAt())
	a.SetLastSeenAt(a.CreatedAt())

	d := dto.AgentToDTO(a)

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(d); err != nil {
		t.Fatal(err)
	}
	var decoded dto.AgentDTO
	if err := gob.NewDecoder(&buf).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	back, err := dto.AgentFromDTO(&decoded)
	if err != nil {
		t.Fatal(err)
	}

	if back.ID() != a.ID() || back.Name() != a.Name() || back.Endpoint() != a.Endpoint() {
		t.Fatalf("id/name/endpoint mismatch: %v", back)
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

func TestUser_Roundtrip(t *testing.T) {
	u, _ := model.NewUser("u1", "subject@x")
	u.EmailAdd("a@b.c")
	u.NameAdd("Alice")
	_ = u.RoleAdd("role-a")
	_ = u.PermissionAdd(kind.Permission("specs:get"))
	u.SetCreatedAt(time.Now().Truncate(time.Microsecond))

	d := dto.UserToDTO(u)
	back, err := dto.UserFromDTO(d)
	if err != nil {
		t.Fatal(err)
	}
	if back.Email() != u.Email() || back.Name() != u.Name() {
		t.Fatalf("email/name mismatch")
	}
	if !back.RoleHas("role-a") {
		t.Fatal("role lost")
	}
	if !back.PermissionHas(kind.Permission("specs:get")) {
		t.Fatal("perm lost")
	}
}

func TestSpec_Roundtrip(t *testing.T) {
	ts, _ := model.NewSpec("s1", "my-spec", "slot-a")
	ts.SetKindType(kind.TaskKindType("subprocess"))
	ts.SetKindConfig(map[string]any{"cmd": "echo"})
	ts.SetTimeoutMs(1000)
	ts.SetTargets([]string{"a1", "a2"})
	ts.SetVersion(3)
	ts.SetGeneration(2)
	ts.MarkForDeletion()
	ts.SetCreatedAt(time.Now().Truncate(time.Microsecond))

	d := dto.SpecToDTO(ts)
	back, err := dto.SpecFromDTO(d)
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

// TestSpec_KindConfigWithNestedMapGobRoundtrip: типичный POST /api/v1/specs
// приходит с KindConfig вида {"env": {...}, "args": [...]} — после JSON-
// unmarshal там живут map[string]interface{} и []interface{}. Проверяем
// что gob их корректно проноcит через Encode/Decode, иначе raft.Apply
// упадёт с "type not registered for interface".
func TestSpec_KindConfigWithNestedMapGobRoundtrip(t *testing.T) {
	dto.Register()

	ts, _ := model.NewSpec("s1", "my-spec", "slot")
	ts.SetKindConfig(map[string]any{
		"cmd":    "echo",
		"args":   []interface{}{"hello", "world"},
		"env":    map[string]interface{}{"KEY": "value", "PORT": 8080},
		"config": map[string]interface{}{"nested": map[string]interface{}{"deep": true}},
	})
	ts.SetCreatedAt(time.Now().Truncate(time.Microsecond))

	d := dto.SpecToDTO(ts)

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(d); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded dto.SpecDTO
	if err := gob.NewDecoder(&buf).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	back, err := dto.SpecFromDTO(&decoded)
	if err != nil {
		t.Fatalf("FromDTO: %v", err)
	}
	kc := back.KindConfig()
	if kc["cmd"] != "echo" {
		t.Fatalf("cmd lost: %v", kc["cmd"])
	}
	env, ok := kc["env"].(map[string]interface{})
	if !ok || env["KEY"] != "value" {
		t.Fatalf("nested env lost: %v", kc["env"])
	}
}

func TestRollout_Roundtrip(t *testing.T) {
	r, _ := model.NewRollout("s1", "a1", 5)
	r.SetIntent(kind.RolloutIntentUpdate)
	r.SetActualTaskID("task-xyz")
	r.MarkSynced(5)

	d := dto.RolloutToDTO(r)
	back, err := dto.RolloutFromDTO(d)
	if err != nil {
		t.Fatal(err)
	}
	if back.ActualTaskID() != "task-xyz" {
		t.Fatalf("task id: %s", back.ActualTaskID())
	}
	if back.Status() != kind.SyncStatusSynced {
		t.Fatalf("status: %v", back.Status())
	}
	if back.ObservedGeneration() != 5 {
		t.Fatalf("observed: %d", back.ObservedGeneration())
	}
}
