package spec

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestDeployAndDeleteAll503Targets(t *testing.T) {
	svc, s := mkService(t)
	ctx := context.Background()
	targets := make([]string, 503)
	for i := range targets {
		targets[i] = fmt.Sprintf("a-%04d", i)
	}
	ts := mkSpec(t, svc, s, "many", targets)
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	all, err := svc.RolloutsBySpec(ctx, ts.ID())
	if err != nil || len(all) != len(targets) {
		t.Fatalf("deploy truncated targets: %d %v", len(all), err)
	}
	if err = svc.Delete(ctx, ts.ID()); err != nil {
		t.Fatal(err)
	}
	all, _ = svc.RolloutsBySpec(ctx, ts.ID())
	for _, ro := range all {
		if ro.Intent() != enum.RolloutIntentUninstall {
			t.Fatalf("target %s not uninstalled", ro.AgentID())
		}
	}
}

func TestLabelSelectorsUnionExplicitAndAllLabels(t *testing.T) {
	svc, s := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, s, "labels", []string{"explicit"})
	for _, id := range []string{"match", "partial", "other"} {
		a, _ := model.NewAgentFrom(model.AgentParams{ID: id, Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1})
		a.LabelAdd("env", "prod")
		if id == "match" {
			a.LabelAdd("team", "ops")
		}
		if id == "other" {
			a.LabelAdd("env", "dev")
		}
		_ = s.UpsertAgent(ctx, a)
	}
	ts.SetTargetLabels(map[string]string{"env": "prod", "team": "ops"})
	_ = svc.Upsert(ctx, ts, 0)
	if err := svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
	all, _ := svc.RolloutsBySpec(ctx, ts.ID())
	if len(all) != 2 {
		t.Fatalf("expected explicit plus ALL-label match, got %d", len(all))
	}
	ts.SetTargets(nil)
	ts.SetTargetLabels(nil)
	_ = svc.Upsert(ctx, ts, 0)
	_ = svc.Deploy(ctx, ts.ID(), "")
	all, _ = svc.RolloutsBySpec(ctx, ts.ID())
	for _, ro := range all {
		if ro.Intent() != enum.RolloutIntentUninstall {
			t.Fatal("empty targets did not uninstall")
		}
	}
}

func TestCreateDoesNotOverwriteExistingSpec(t *testing.T) {
	svc, s := mkService(t)
	ts := mkSpec(t, svc, s, "exists", nil)
	if err := svc.Create(context.Background(), ts); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate create silently overwrote: %v", err)
	}
}

func TestUpsertUsesStoredCountersAndGenerationForDeploy(t *testing.T) {
	svc, s := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, s, "counter", []string{"a"})
	ts.SetVersion(99)
	ts.SetGeneration(88)
	ts.SetName("rename")
	if err := svc.Upsert(ctx, ts, 1); err != nil {
		t.Fatal(err)
	}
	if ts.Version() != 2 || ts.Generation() != 1 {
		t.Fatal("client counters trusted")
	}
	_ = svc.Deploy(ctx, ts.ID(), "")
	all, _ := svc.RolloutsBySpec(ctx, ts.ID())
	if all[0].DesiredGeneration() != 1 {
		t.Fatal("deployment used edit counter instead of runtime generation")
	}
}

func TestDeployRejectsIncompatibleAdvertisedRunner(t *testing.T) {
	svc, s := mkService(t)
	ctx := context.Background()
	ts := mkSpec(t, svc, s, "routing", []string{"a"})
	ag, _ := s.GetAgent(ctx, "a")
	ag.SetCapabilities([]model.RunnerCapability{{Name: "wasm", Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Wasm"}}}})
	_ = s.UpsertAgent(ctx, ag)
	err := svc.Deploy(ctx, ts.ID(), "")
	var unsupported *UnsupportedTargetsError
	if !errors.As(err, &unsupported) {
		t.Fatalf("incompatible advertised runner accepted: %v", err)
	}
	ag.SetCapabilities([]model.RunnerCapability{{Name: "sub", Labels: map[string]string{"zone": "a"}, Workloads: []model.GroupVersionKind{{Group: "solti.io", Version: "v1", Kind: "Subprocess"}}}})
	_ = s.UpsertAgent(ctx, ag)
	ts.SetRunnerLabels(map[string]string{"zone": "b"})
	_ = svc.Upsert(ctx, ts, 0)
	err = svc.Deploy(ctx, ts.ID(), "")
	if !errors.As(err, &unsupported) {
		t.Fatal("runner selector ignored")
	}
	ts.SetRunnerLabels(map[string]string{"zone": "a"})
	_ = svc.Upsert(ctx, ts, 0)
	if err = svc.Deploy(ctx, ts.ID(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSpecRejectedBeforePersistence(t *testing.T) {
	svc, _ := mkService(t)
	s, _ := model.NewSpec("bad", "name", "slot")
	if err := svc.Create(context.Background(), s); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("invalid missing workload persisted: %v", err)
	}
}
