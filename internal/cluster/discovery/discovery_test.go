package discovery_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
)

// New builds the driver its config selects: static by default, with the parsed
// peers, or dns. An unknown driver and a malformed peer are refused.
func TestNew(t *testing.T) {
	static := []string{"n1=10.0.0.1:7000", "10.0.0.2:7000"}
	parsed := []cluster.Peer{{ID: "n1", Address: "10.0.0.1:7000"}, {ID: "10.0.0.2:7000", Address: "10.0.0.2:7000"}}
	for _, tc := range []struct {
		name  string
		cfg   cluster.DiscoveryConfig
		check func(t *testing.T, d cluster.Discovery, err error)
	}{
		{"static by default", cluster.DiscoveryConfig{Peers: static}, staticPeers(parsed)},
		{"static", cluster.DiscoveryConfig{Driver: cluster.DriverStatic, Peers: static}, staticPeers(parsed)},
		{"dns", cluster.DiscoveryConfig{Driver: cluster.DriverDNS, Hostname: "podium.svc", Port: 7000}, func(t *testing.T, d cluster.Discovery, err error) {
			if _, ok := d.(*discovery.DNS); err != nil || !ok {
				t.Fatalf("New() = %T, %v; want *discovery.DNS", d, err)
			}
		}},
		{"unknown driver", cluster.DiscoveryConfig{Driver: "consul"}, func(t *testing.T, d cluster.Discovery, err error) {
			if err == nil {
				t.Fatalf("New() = %T, want an error", d)
			}
		}},
		{"malformed peer", cluster.DiscoveryConfig{Peers: []string{"n1="}}, func(t *testing.T, d cluster.Discovery, err error) {
			if !errors.Is(err, cluster.ErrInvalidPeer) {
				t.Fatalf("New() = %T, %v; want %v", d, err, cluster.ErrInvalidPeer)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := discovery.New(tc.cfg)
			tc.check(t, d, err)
		})
	}
}

// staticPeers checks that New built a static driver that reports want.
func staticPeers(want []cluster.Peer) func(*testing.T, cluster.Discovery, error) {
	return func(t *testing.T, d cluster.Discovery, err error) {
		t.Helper()
		if _, ok := d.(*discovery.Static); err != nil || !ok {
			t.Fatalf("New() = %T, %v; want *discovery.Static", d, err)
		}
		if got, err := d.Peers(context.Background()); err != nil || !slices.Equal(got, want) {
			t.Fatalf("Peers() = %v, %v; want %v", got, err, want)
		}
	}
}
