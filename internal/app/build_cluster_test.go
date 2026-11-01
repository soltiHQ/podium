package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
			if _, ok := store.(storage.Scanner); !ok {
				t.Fatalf("%T is not a storage.Scanner: every scan of it would page", store)
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

// The first start on a single-blob file logs the migration once, at WARN, with the
// data directory, the new format and the fact that older versions refuse the file.
// The next start finds a file in format 2 and logs no such warning.
func TestBuildClusterLogsTheMigrationOnce(t *testing.T) {
	fixture, err := os.ReadFile("../storage/durable/testdata/single-blob.db")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, durable.FileName), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cluster.DefaultConfig()
	cfg.Standalone.DataDir = dir

	for _, start := range []struct {
		name  string
		warns int
	}{{"first start", 1}, {"second start", 0}} {
		var logs bytes.Buffer
		_, _, hub, shutdown, err := buildCluster(cfg, zerolog.New(&logs))
		if err != nil {
			t.Fatalf("%s: %v", start.name, err)
		}
		hub.Close()
		shutdown()

		out := logs.String()
		if warns := strings.Count(out, `"level":"warn"`); warns != start.warns {
			t.Fatalf("%s logged %d warnings, want %d: %s", start.name, warns, start.warns, out)
		}
		if start.warns == 0 {
			continue
		}
		for _, want := range []string{dir, "single-blob", "format 2", "older versions refuse the migrated file"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: the warning lacks %q: %s", start.name, want, out)
			}
		}
	}
}

func TestBuildClusterRefusesImplicitEphemeral(t *testing.T) {
	cfg := cluster.DefaultConfig()
	cfg.Standalone.DataDir = ""
	if _, _, _, _, err := buildCluster(cfg, zerolog.Nop()); err == nil {
		t.Fatal("blank data directory silently selected ephemeral state")
	}
}
