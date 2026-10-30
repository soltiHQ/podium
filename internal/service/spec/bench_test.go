package spec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// benchStore opens a durable store in a fresh directory and seeds it.
func benchStore(b *testing.B, f storagetest.Fixture) *durable.Store {
	b.Helper()
	s, err := durable.Open(filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err = storagetest.Seed(context.Background(), s, f); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkPlacementPreview is the preview a spec page computes on every
// refresh. A label spec reads the whole fleet; a spec that lists its agents
// reads those only, but still the rollouts of the spec.
func BenchmarkPlacementPreview(b *testing.B) {
	for _, tc := range []struct {
		name    string
		agents  int
		spec    string
		targets int
	}{
		{"explicit20", 5000, storagetest.ExplicitSpec, storagetest.ExplicitTargets},
		{"labels1000", 1000, storagetest.FleetSpec, 1000},
		{"labels5000", 5000, storagetest.FleetSpec, 5000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			svc := New(benchStore(b, storagetest.Fixture{Agents: tc.agents}), zerolog.Nop())
			plan, err := svc.PreviewPlacement(ctx, tc.spec)
			if err != nil {
				b.Fatal(err)
			}
			if len(plan.Targets) != tc.targets || !plan.DeployAllowed {
				b.Fatalf("%s plans %d targets, deployable %t, want %d deployable", tc.spec, len(plan.Targets), plan.DeployAllowed, tc.targets)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := svc.PreviewPlacement(ctx, tc.spec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
