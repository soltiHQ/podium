package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/service"
	agentsvc "github.com/soltiHQ/control-plane/internal/service/agent"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	workloadsvc "github.com/soltiHQ/control-plane/internal/service/workload"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/workloadschema"
)

// Two schemas of the Container type: the body {"image": "busybox"} fits
// fitsSchema and not portSchema, which asks for a port.
const (
	fitsSchema = `{"type":"object","properties":{"image":{"type":"string"}}}`
	portSchema = `{"type":"object","required":["image","port"],"properties":{"image":{"type":"string"},"port":{"type":"integer"}}}`
	// otherFitsSchema is a third schema the body fits, under another fingerprint.
	otherFitsSchema = `{"type":"object","properties":{"image":{"type":"string","minLength":1}}}`
)

// catalogSchema stores schema in the workload catalog and returns its fingerprint.
func catalogSchema(t *testing.T, s storage.Storage, schema string) string {
	t.Helper()
	fp, err := workloadschema.Fingerprint([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := model.NewWorkloadSchema(fp, "solti.io/v1", "Container", []byte(schema), "agent-x", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertWorkloadSchema(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	return fp
}

// compatSpec stores spec specID on agentID, whose workload names the schema of
// fingerprint, with the body {"image": "busybox"}.
func compatSpec(t *testing.T, s storage.Storage, specID, agentID, fingerprint string) *model.Spec {
	t.Helper()
	ts, err := model.NewSpec(specID, "n", "slot-"+specID)
	if err != nil {
		t.Fatal(err)
	}
	ts.SetTargets([]string{agentID})
	ts.SetWorkload(model.Workload{APIVersion: "solti.io/v1", Kind: "Container", SchemaFingerprint: fingerprint, Body: map[string]any{"image": "busybox"}})
	if err = s.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

// declareContainer stores agentID, as a sync of an agent whose one runner
// declares the Container type with fingerprint makes it.
func declareContainer(t *testing.T, s storage.Storage, agentID, fingerprint string) {
	t.Helper()
	ag, err := model.NewAgentFrom(model.AgentParams{
		ID: agentID, Name: "a-" + agentID, Endpoint: "http://" + agentID, EndpointType: 2, APIVersion: 1,
		Capabilities: []model.RunnerCapability{{Name: "runner", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Container", SchemaFingerprint: fingerprint}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertAgent(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
}

// Every push checks its target first, as placement does. A target whose schema
// changed after the deploy to one that refuses the body gets no call: an install
// and an update are marked failed with the reason and the problems, and the task
// an update would replace is left as it runs. A target whose schema is the spec's
// own, or another the body fits, is pushed.
func TestEveryPushChecksTheTargetFirst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		declare func(t *testing.T, s storage.Storage) string // the fingerprint the agent declares
		pushed  bool
	}{
		{"the spec's own schema", func(t *testing.T, s storage.Storage) string { return catalogSchema(t, s, fitsSchema) }, true},
		{"another schema the body fits", func(t *testing.T, s storage.Storage) string { return catalogSchema(t, s, otherFitsSchema) }, true},
		{"a schema that refuses the body", func(t *testing.T, s storage.Storage) string { return catalogSchema(t, s, portSchema) }, false},
	} {
		for _, intent := range []enum.RolloutIntent{enum.RolloutIntentInstall, enum.RolloutIntentUpdate} {
			t.Run(tc.name+", "+intent.String(), func(t *testing.T) {
				ctx := context.Background()
				fp := &fakeProxy{applyResp: []string{"task-new"}}
				r, s, _ := newFakeRunner(t, fp)
				own := catalogSchema(t, s, fitsSchema)
				ts := compatSpec(t, s, "sp-1", "agent-a", own)
				declareContainer(t, s, "agent-a", tc.declare(t, s))
				ro, err := model.NewRollout(ts.ID(), "agent-a", 1)
				if err != nil {
					t.Fatal(err)
				}
				if intent == enum.RolloutIntentUpdate {
					// The task of generation 1 runs; the deploy of generation 2 came after.
					ro.SetActualTaskID("task-running")
					ro.MarkSynced(1)
					ts.BumpGeneration()
					if err = s.UpsertSpec(ctx, ts); err != nil {
						t.Fatal(err)
					}
					ro.SetIntent(enum.RolloutIntentUpdate)
					ro.MarkPending(2)
				} else {
					ro.SetIntent(enum.RolloutIntentInstall)
				}
				if err = s.UpsertRollout(ctx, ro); err != nil {
					t.Fatal(err)
				}

				r.tick(ctx)

				after := mustRollout(t, s, ro.ID())
				if tc.pushed {
					if len(fp.applies) != 1 || after.Status() != enum.SyncStatusSynced {
						t.Fatalf("a compatible target: %d pushes, status %s %q, want one push, synced", len(fp.applies), after.Status(), after.Error())
					}
					return
				}
				if len(fp.applies) != 0 || len(fp.deletes) != 0 {
					t.Fatalf("an incompatible target got %d pushes and %d deletes, want none", len(fp.applies), len(fp.deletes))
				}
				if after.Status() != enum.SyncStatusFailed || !strings.Contains(after.Error(), "schema_incompatible") || !strings.Contains(after.Error(), "port") {
					t.Fatalf("the rollout is %s: %q, want it failed with schema_incompatible and the problem", after.Status(), after.Error())
				}
				if intent == enum.RolloutIntentUpdate && after.ActualTaskID() != "task-running" {
					t.Fatalf("the running task is %q, want it left as task-running", after.ActualTaskID())
				}
			})
		}
	}
}

// The rolling-upgrade case: an agent restarts on a version whose schema
// refuses the body, and its tasks are gone. The observation queues the lost task
// for a reinstall, and the push that follows checks the target and re-installs
// nothing.
func TestRecoveryReinstallsNothingOnAnIncompatibleTarget(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProxy{applyResp: []string{"task-new"}}
	fp.hold("other-task") // the restarted agent runs, without the rollout's task
	r, s, _ := newFakeRunner(t, fp)
	own := catalogSchema(t, s, fitsSchema)
	ts := compatSpec(t, s, "sp-1", "agent-a", own)
	declareContainer(t, s, "agent-a", catalogSchema(t, s, portSchema))
	ro, err := model.NewRollout(ts.ID(), "agent-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	ro.SetActualTaskID("task-lost")
	ro.MarkSynced(1)
	if err = s.UpsertRollout(ctx, ro); err != nil {
		t.Fatal(err)
	}

	r.tick(ctx) // observes: the task is missing, so it is queued for a reinstall
	if queued := mustRollout(t, s, ro.ID()); queued.Intent() != enum.RolloutIntentInstall || queued.ActualTaskID() != "" {
		t.Fatalf("the lost task was not queued for a reinstall: intent %s, task %q", queued.Intent(), queued.ActualTaskID())
	}
	r.tick(ctx) // the reinstall

	if len(fp.applies) != 0 || len(fp.deletes) != 0 {
		t.Fatalf("the incompatible target got %d pushes and %d deletes, want none", len(fp.applies), len(fp.deletes))
	}
	after := mustRollout(t, s, ro.ID())
	if after.Status() != enum.SyncStatusFailed || !strings.Contains(after.Error(), "schema_incompatible") {
		t.Fatalf("the reinstall is %s: %q, want it failed with schema_incompatible", after.Status(), after.Error())
	}
}

// The runner is built with the workload catalog it checks targets against, and
// refuses to be built without one, as without its other services.
func TestNewRefusesANilCatalog(t *testing.T) {
	s := inmemory.New()
	history, err := historysvc.New(s, historysvc.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool := proxy.NewPool(nil, true)
	t.Cleanup(func() { _ = pool.Close() })
	hub := event.NewHub(zerolog.Nop())
	t.Cleanup(hub.Close)
	agents := agentsvc.New(s, zerolog.Nop())
	if _, err = New(Config{}, zerolog.Nop(), s, pool, agents, nil, hub, history, brief{}); !errors.Is(err, service.ErrNilService) {
		t.Fatalf("New without a catalog = %v, want ErrNilService", err)
	}
	r, err := New(Config{}, zerolog.Nop(), s, pool, agents, workloadsvc.New(s, zerolog.Nop()), hub, history, brief{})
	if err != nil || r.schemas == nil {
		t.Fatalf("New with a catalog = %v, schemas %v", err, r)
	}
}
