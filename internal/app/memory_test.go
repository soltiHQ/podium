package app

import (
	"context"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
)

// An app on the memory backend, the default, serves: /readyz answers 200 and a
// user written to its store is read back.
func TestMemoryBackendServes(t *testing.T) {
	r := startApp(t, func(c *config.Config) { c.Cluster.Backend = cluster.BackendMemory }, nil)
	ops := r.app.ops.Addr
	waitFor(t, 10*time.Second, "the ops listener binds", func() bool { return ops() != "" })
	opsAnswers(t, ops(), "/readyz", 200, "ok")

	ctx := context.Background()
	u, err := model.NewUser("memory-user", "memory-user")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.app.store.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := r.app.store.GetUser(ctx, "memory-user")
	if err != nil || got.ID() != "memory-user" {
		t.Fatalf("user read back = %v, %v", got, err)
	}
}
