package snapshot_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// nul is the NUL character, which a Postgres text or jsonb value refuses and
// the JSON of an entity must carry: every string below holds one.
const nul = "\x00"

// created lies in a zone other than UTC and between whole microseconds, so
// that a codec which kept less than nanoseconds would fail.
var created = time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("test", 3*60*60))

// codec is the JSON codec of one entity, and the snapshot message the entity
// is compared through: the message carries every field a snapshot persists.
type codec[T any] struct {
	marshal   func(T) ([]byte, error)
	unmarshal func([]byte) (T, error)
	message   func(*testing.T, T) proto.Message
}

// roundTrip encodes e, checks that the JSON holds each NUL as the escape \u0000
// and no NUL byte, decodes it, and compares the entity read back with e through
// its snapshot message.
func roundTrip[T any](t *testing.T, c codec[T], e T) {
	t.Helper()
	b, err := c.marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(b) {
		t.Fatalf("marshal: not JSON: %q", b)
	}
	if !bytes.Contains(b, []byte(`\u0000`)) || bytes.IndexByte(b, 0) >= 0 {
		t.Errorf("marshal: %q, want each NUL as the escape \\u0000 and no NUL byte", b)
	}
	got, err := c.unmarshal(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gotMsg, wantMsg := c.message(t, got), c.message(t, e); !proto.Equal(gotMsg, wantMsg) {
		t.Errorf("round trip:\n got %v\nwant %v", gotMsg, wantMsg)
	}
}

var (
	agentCodec = codec[*model.Agent]{
		snapshot.MarshalAgentJSON, snapshot.UnmarshalAgentJSON,
		func(_ *testing.T, a *model.Agent) proto.Message { return wire.AgentToProto(a) },
	}
	agentCredentialCodec = codec[*model.AgentCredential]{
		snapshot.MarshalAgentCredentialJSON, snapshot.UnmarshalAgentCredentialJSON,
		func(_ *testing.T, c *model.AgentCredential) proto.Message { return wire.AgentCredentialToProto(c) },
	}
	userCodec = codec[*model.User]{
		snapshot.MarshalUserJSON, snapshot.UnmarshalUserJSON,
		func(_ *testing.T, u *model.User) proto.Message { return wire.UserToProto(u) },
	}
	credentialCodec = codec[*model.Credential]{
		snapshot.MarshalCredentialJSON, snapshot.UnmarshalCredentialJSON,
		func(_ *testing.T, c *model.Credential) proto.Message { return wire.CredentialToProto(c) },
	}
	verifierCodec = codec[*model.Verifier]{
		snapshot.MarshalVerifierJSON, snapshot.UnmarshalVerifierJSON,
		func(_ *testing.T, v *model.Verifier) proto.Message { return wire.VerifierToProto(v) },
	}
	sessionCodec = codec[*model.Session]{
		snapshot.MarshalSessionJSON, snapshot.UnmarshalSessionJSON,
		func(_ *testing.T, s *model.Session) proto.Message { return wire.SessionToProto(s) },
	}
	roleCodec = codec[*model.Role]{
		snapshot.MarshalRoleJSON, snapshot.UnmarshalRoleJSON,
		func(_ *testing.T, r *model.Role) proto.Message { return wire.RoleToProto(r) },
	}
	specCodec = codec[*model.Spec]{
		snapshot.MarshalSpecJSON, snapshot.UnmarshalSpecJSON,
		func(t *testing.T, s *model.Spec) proto.Message {
			m, err := wire.SpecToProto(s)
			if err != nil {
				t.Fatalf("the spec's snapshot message: %v", err)
			}
			return m
		},
	}
	rolloutCodec = codec[*model.Rollout]{
		snapshot.MarshalRolloutJSON, snapshot.UnmarshalRolloutJSON,
		func(_ *testing.T, r *model.Rollout) proto.Message { return wire.RolloutToProto(r) },
	}
	executionCodec = codec[*model.ExecutionRecord]{
		snapshot.MarshalExecutionRecordJSON, snapshot.UnmarshalExecutionRecordJSON,
		func(_ *testing.T, e *model.ExecutionRecord) proto.Message { return wire.ExecutionToProto(e) },
	}
)

// TestEntityJSONRoundTrip writes every entity, each field set and a NUL in
// each string, and reads it back.
func TestEntityJSONRoundTrip(t *testing.T) {
	t.Run("agent", func(t *testing.T) {
		a, err := model.NewAgent("agent"+nul+"a", "edge"+nul+"name", "https://edge"+nul+".example.net:8443")
		if err != nil {
			t.Fatal(err)
		}
		a.SetEndpointType(enum.EndpointHTTP)
		a.SetAPIVersion(1)
		a.SetOS("linux" + nul)
		a.SetArch("arm64" + nul)
		a.SetPlatform("ubuntu" + nul)
		a.SetUptimeSeconds(3600)
		a.SetHeartbeatInterval(15 * time.Second)
		a.SetStatus(enum.AgentStatusDisconnected)
		a.SetMetadata(map[string]string{"host" + nul: "edge" + nul + ".dc1"})
		a.SetLabels(map[string]string{"env" + nul: "prod" + nul})
		a.SetCapabilities([]model.RunnerCapability{{
			Name:      "subprocess" + nul,
			Labels:    map[string]string{"pool" + nul: "gpu" + nul},
			Workloads: []model.GroupVersionKind{{Group: "solti.io" + nul, Version: "v1", Kind: "Subprocess"}},
		}})
		a.SetLastSeenAt(created.Add(time.Minute))
		a.SetStaleAt(created.Add(2 * time.Minute))
		a.SetCreatedAt(created)
		a.SetUpdatedAt(created.Add(3 * time.Minute))
		roundTrip(t, agentCodec, a)
	})
	t.Run("stored agent credential", func(t *testing.T) {
		c, err := model.NewStoredAgentCredential("agent"+nul+"a", "token"+nul+"raw")
		if err != nil {
			t.Fatal(err)
		}
		c.SetCreatedAt(created)
		c.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, agentCredentialCodec, c)
	})
	t.Run("derived agent credential", func(t *testing.T) {
		c, err := model.NewDerivedAgentCredential("agent"+nul+"b", 7, []byte{0xa0, 0, 0x5c})
		if err != nil {
			t.Fatal(err)
		}
		c.SetCreatedAt(created)
		c.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, agentCredentialCodec, c)
	})
	t.Run("user", func(t *testing.T) {
		u, err := model.NewUser("user"+nul+"a", "sub"+nul+"a")
		if err != nil {
			t.Fatal(err)
		}
		u.SetEmail("user" + nul + "@example.net")
		u.SetName("User" + nul + "A")
		u.SetRoleIDs([]string{"role" + nul + "a", "role-b"})
		u.SetPermissions([]string{string(enum.All[0]), string(enum.All[5])})
		u.Disable()
		u.SetCreatedAt(created)
		u.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, userCodec, u)
	})
	t.Run("credential", func(t *testing.T) {
		c, err := model.NewCredential("cred"+nul+"a", "user"+nul+"a", enum.Auth("password"+nul))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetSecret("hash"+nul, "digest"+nul); err != nil {
			t.Fatal(err)
		}
		c.SetCreatedAt(created)
		c.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, credentialCodec, c)
	})
	t.Run("verifier", func(t *testing.T) {
		v, err := model.NewVerifier("ver"+nul+"a", "cred"+nul+"a", enum.Password)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.DataSet("argon2id"+nul, "digest"+nul); err != nil {
			t.Fatal(err)
		}
		v.SetCreatedAt(created)
		v.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, verifierCodec, v)
	})
	t.Run("session", func(t *testing.T) {
		s, err := model.NewSession("sess"+nul+"a", "user"+nul+"a", "cred"+nul+"a", enum.APIKey, []byte{0, 1, 2}, created.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.SetRevokedAt(created.Add(2 * time.Hour))
		s.SetCreatedAt(created)
		s.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, sessionCodec, s)
	})
	t.Run("session modified at the Unix epoch", func(t *testing.T) {
		// A session's message says which of its times are set, so the epoch comes
		// back the epoch, not the zero time.
		s, err := model.NewSession("sess"+nul+"b", "user-b", "cred-b", "", []byte{0}, created.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.SetCreatedAt(created)
		s.SetUpdatedAt(time.Unix(0, 0))
		roundTrip(t, sessionCodec, s)
		b, err := sessionCodec.marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := sessionCodec.unmarshal(b)
		if err != nil || got.UpdatedAt().IsZero() || !got.UpdatedAt().Equal(time.Unix(0, 0)) {
			t.Errorf("UpdatedAt = %v, %v; want the Unix epoch", got.UpdatedAt(), err)
		}
	})
	t.Run("role", func(t *testing.T) {
		r, err := model.NewRole("role"+nul+"a", "admins"+nul)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(r.PermissionAdd(enum.All[2]), r.PermissionAdd(enum.All[7])); err != nil {
			t.Fatal(err)
		}
		r.SetCreatedAt(created)
		r.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, roleCodec, r)
	})
	t.Run("spec", func(t *testing.T) {
		s, err := model.NewSpec("spec"+nul+"a", "job"+nul, "slot"+nul)
		if err != nil {
			t.Fatal(err)
		}
		s.SetKindType(enum.TaskKindContainer)
		s.SetKindConfig(map[string]any{
			"image": "registry.example.net/job" + nul,
			"args":  []any{"--shard" + nul, 1.5},
			"env":   map[string]any{"MODE" + nul: "fast" + nul},
		})
		s.SetTimeoutMs(45000)
		s.SetRestartType(enum.RestartOnFailure)
		s.SetIntervalMs(60000)
		s.SetBackoff(model.BackoffConfig{Jitter: enum.JitterFull, Factor: 1.5, FirstMs: 250, MaxMs: 9000})
		s.SetTargets([]string{"agent" + nul + "a"})
		s.SetTargetLabels(map[string]string{"env" + nul: "prod" + nul})
		s.SetRunnerLabels(map[string]string{"pool" + nul: "gpu" + nul})
		s.SetVersion(4)
		s.SetGeneration(3)
		s.SetDeletionRequested(true)
		s.SetCreatedAt(created)
		s.SetUpdatedAt(created.Add(time.Minute))
		roundTrip(t, specCodec, s)
	})
	t.Run("rollout", func(t *testing.T) {
		r, err := model.NewRollout("spec"+nul+"a", "agent"+nul+"a", 5)
		if err != nil {
			t.Fatal(err)
		}
		r.SetActualTaskID("task" + nul)
		r.RestoreState(enum.SyncStatusFailed, enum.RolloutIntentUpdate, 4, 2, "apply failed"+nul,
			created.Add(time.Minute), created.Add(2*time.Minute))
		r.SetCreatedAt(created)
		r.SetUpdatedAt(created.Add(3 * time.Minute))
		roundTrip(t, rolloutCodec, r)
	})
	t.Run("execution record", func(t *testing.T) {
		exit := int32(0)
		e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
			AgentID: "agent" + nul + "a", AgentName: "edge" + nul, TaskName: "build" + nul, TaskUID: "uid" + nul,
			Generation: 3, Attempt: 2, Status: "failed", StartedAtMs: 1000, FinishedAtMs: 2000, ExitCode: &exit,
			Error: "exit" + nul, WorkloadAPIVersion: "solti.io/v1" + nul, WorkloadKind: "Container" + nul,
			SpecID: "spec" + nul, SpecName: "job" + nul, SourceResourceVersion: "42" + nul, CapturedAt: created,
		})
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, executionCodec, e)
	})
}

// TestEntityJSONRefusesWhatItCannotCarry checks that a Marshal function refuses,
// with storage.ErrInvalidArgument, an entity its JSON would not carry exactly:
// nil, a time outside the years 1678 to 2262, which a message holds in Unix
// nanoseconds, the Unix epoch where a message reads 0 back as the zero time, and
// a string that is not valid UTF-8, which a proto3 string field refuses.
func TestEntityJSONRefusesWhatItCannotCarry(t *testing.T) {
	agent := func(t *testing.T, change func(*model.Agent)) *model.Agent {
		a, err := model.NewAgent("agent-a", "edge", "https://edge.example.net")
		if err != nil {
			t.Fatal(err)
		}
		a.SetCreatedAt(created)
		change(a)
		return a
	}
	for _, tc := range []struct {
		name    string
		marshal func(*testing.T) ([]byte, error)
	}{
		{"a nil agent", func(*testing.T) ([]byte, error) { return snapshot.MarshalAgentJSON(nil) }},
		{"a nil execution record", func(*testing.T) ([]byte, error) { return snapshot.MarshalExecutionRecordJSON(nil) }},
		{"a creation time after 2262", func(t *testing.T) ([]byte, error) {
			return snapshot.MarshalAgentJSON(agent(t, func(a *model.Agent) { a.SetCreatedAt(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)) }))
		}},
		{"a staleness deadline before 1678", func(t *testing.T) ([]byte, error) {
			return snapshot.MarshalAgentJSON(agent(t, func(a *model.Agent) { a.SetStaleAt(time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)) }))
		}},
		{"a last sync at the Unix epoch", func(t *testing.T) ([]byte, error) {
			return snapshot.MarshalAgentJSON(agent(t, func(a *model.Agent) { a.SetLastSeenAt(time.Unix(0, 0)) }))
		}},
		{"a session expiring after 2262", func(t *testing.T) ([]byte, error) {
			s, err := model.NewSession("sess-a", "user-a", "cred-a", enum.Password, []byte{1}, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			s.SetCreatedAt(created)
			return snapshot.MarshalSessionJSON(s)
		}},
		{"a string that is not valid UTF-8", func(t *testing.T) ([]byte, error) {
			return snapshot.MarshalAgentJSON(agent(t, func(a *model.Agent) { a.SetOS("linux\xff") }))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := tc.marshal(t); !errors.Is(err, storage.ErrInvalidArgument) {
				t.Errorf("marshal: %q, %v; want storage.ErrInvalidArgument", b, err)
			}
		})
	}
}

// TestEntityJSONRefusesACorruptBody checks that an Unmarshal function answers
// storage.ErrInternal for a body that is not the JSON of its message, or that
// decodes to an entity no store keeps, and that the error carries none of the
// body.
func TestEntityJSONRefusesACorruptBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{"id": "secret-token-123"`},
		{"a field of the wrong type", `{"agentId": 42, "token": "secret-token-123"}`},
		{"no ID", `{"token": "secret-token-123", "createdAtNs": "1"}`},
		{"no creation time", `{"agentId": "agent-a", "token": "secret-token-123"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := snapshot.UnmarshalAgentCredentialJSON([]byte(tc.body))
			if !errors.Is(err, storage.ErrInternal) {
				t.Fatalf("unmarshal: %v, want storage.ErrInternal", err)
			}
			if bytes.Contains([]byte(err.Error()), []byte("secret-token")) {
				t.Errorf("unmarshal: %q carries the body", err)
			}
		})
	}
}
