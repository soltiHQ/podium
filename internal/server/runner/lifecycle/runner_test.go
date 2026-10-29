package lifecycle

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

type heartbeatBeforeTx struct {
	storage.Storage
	fresh *model.Agent
}

func (s *heartbeatBeforeTx) WithTx(ctx context.Context, fn func(storage.Storage) error) error {
	_ = s.Storage.UpsertAgent(ctx, s.fresh)
	return s.Storage.WithTx(ctx, fn)
}

func TestFreshHeartbeatNotOverwrittenByStaleTick(t *testing.T) {
	s := inmemory.New()
	old, _ := model.NewAgentFrom(model.AgentParams{ID: "a", EndpointType: 2, APIVersion: 1})
	old.SetLastSeenAt(time.Now().Add(-time.Hour))
	_ = s.UpsertAgent(context.Background(), old)
	fresh := old.Clone()
	fresh.SetLastSeenAt(time.Now())
	r := &Runner{store: &heartbeatBeforeTx{Storage: s, fresh: fresh}, cfg: Config{}.withDefaults(), logger: zerolog.New(io.Discard), hub: event.NewHub(zerolog.New(io.Discard))}
	defer r.hub.Close()
	r.reconcile(context.Background(), time.Now(), old)
	got, err := s.GetAgent(context.Background(), "a")
	if err != nil || got.Status() != enum.AgentStatusActive || !got.LastSeenAt().Equal(fresh.LastSeenAt()) {
		t.Fatal("stale tick deleted or overwrote reconnected agent")
	}
}

func TestLifecycleScansAll503StaleAgents(t *testing.T) {
	s := inmemory.New()
	ctx := context.Background()
	for i := 0; i < 503; i++ {
		a, _ := model.NewAgentFrom(model.AgentParams{ID: fmt.Sprintf("a-%04d", i), EndpointType: 2, APIVersion: 1})
		a.SetLastSeenAt(time.Now().Add(-time.Hour))
		a.SetStaleAt(time.Now().Add(-time.Hour))
		_ = s.UpsertAgent(ctx, a)
	}
	r := &Runner{store: s, cfg: Config{}.withDefaults(), logger: zerolog.New(io.Discard), hub: event.NewHub(zerolog.New(io.Discard))}
	defer r.hub.Close()
	r.tick(ctx)
	left, err := storage.AllAgents(ctx, s, nil)
	if err != nil || len(left) != 0 {
		t.Fatalf("old stale page skipped %d %v", len(left), err)
	}
}
