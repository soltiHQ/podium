package agent

import (
	"context"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func TestReconnectResetsFailedRetryBudgetButHeartbeatDoesNot(t *testing.T) {
	ctx := context.Background()
	s := inmemory.New()
	svc := New(s, zerolog.New(io.Discard))
	a, _ := model.NewAgentFrom(model.AgentParams{ID: "a", Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1, UptimeSeconds: 100})
	_ = svc.Upsert(ctx, a)
	ro, _ := model.NewRollout("spec", "a", 3)
	for i := 0; i < 5; i++ {
		ro.MarkFailed("offline")
	}
	_ = s.UpsertRollout(ctx, ro)
	heartbeat, _ := model.NewAgentFrom(model.AgentParams{ID: "a", Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1, UptimeSeconds: 101})
	_ = svc.Upsert(ctx, heartbeat)
	got, _ := s.GetRollout(ctx, ro.ID())
	if got.Attempts() != 5 {
		t.Fatal("ordinary heartbeat reset retry budget")
	}
	restart, _ := model.NewAgentFrom(model.AgentParams{ID: "a", Endpoint: "http://127.0.0.1:8085", EndpointType: 2, APIVersion: 1, UptimeSeconds: 1})
	if err := svc.Upsert(ctx, restart); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRollout(ctx, ro.ID())
	if got.Status() != enum.SyncStatusPending || got.Attempts() != 0 || got.DesiredGeneration() != 3 {
		t.Fatal("restart did not recover exhausted rollout")
	}
}
