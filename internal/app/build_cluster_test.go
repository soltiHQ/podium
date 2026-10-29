package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
)

func TestBuildStandaloneDurableAndEphemeral(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "durable"}[persistent], func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			cfg.Standalone.Ephemeral = !persistent
			if persistent {
				cfg.Standalone.DataDir = filepath.Join(t.TempDir(), "state")
			}
			store, leader, hub, shutdown, err := buildCluster(cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			if !leader.AmLeader() {
				t.Fatal("standalone must be leader")
			}
			_, isDurable := store.(*durable.Store)
			if isDurable != persistent {
				t.Fatal("wrong standalone backend")
			}
			ctx := context.Background()
			agent, _ := model.NewAgent("a1", "fixture", "http://fixture")
			if err := store.UpsertAgent(ctx, agent); err != nil {
				t.Fatal(err)
			}
			hub.Close()
			if shutdown != nil {
				shutdown()
			}
			store, _, hub, shutdown, err = buildCluster(cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			if shutdown != nil {
				defer shutdown()
			}
			_, err = store.GetAgent(ctx, "a1")
			if persistent && err != nil {
				t.Fatalf("durable restart lost state: %v", err)
			}
			if !persistent && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("ephemeral restart: %v", err)
			}
		})
	}
}

func TestBuildClusterRefusesImplicitEphemeral(t *testing.T) {
	cfg := cluster.DefaultConfig()
	cfg.Standalone.DataDir = ""
	if _, _, _, _, err := buildCluster(cfg, zerolog.Nop()); err == nil {
		t.Fatal("blank data directory silently selected ephemeral state")
	}
}
