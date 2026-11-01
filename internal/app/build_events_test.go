package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
)

// burst sends 5 refresh pulses at once and returns what the client got
// right away. The first delivery is the one left in the channel.
func burst(hub *event.Hub, pulses <-chan string) int {
	for range 5 {
		hub.Notify(event.RefreshAgents)
	}
	return len(pulses)
}

// A zero events.coalesce keeps the default window: the hub is never silently
// left without one, which would send a pulse per notification again.
func TestHubOptionsKeepTheDefaultWindowForZero(t *testing.T) {
	cfg := config.Default()
	cfg.Events.Coalesce = 0
	cc := cluster.DefaultConfig()
	cc.Standalone.Ephemeral = true
	_, _, hub, _, err := buildCluster(cc, zerolog.Nop(), hubOptions(cfg)...)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	pulses := hub.Subscribe(t.Context())

	if n := burst(hub, pulses); n != 1 {
		t.Fatalf("%d of 5 pulses delivered at once, want only the first", n)
	}
	<-pulses
	// The default window is long: the trailing delivery is not due yet.
	select {
	case <-pulses:
		t.Fatalf("a second delivery came before the default window of %v ended", event.DefaultCoalesce)
	case <-time.After(event.DefaultCoalesce / 4):
	}
}

// The app builds its hub with the window of events.coalesce: the trailing
// delivery of a burst comes at the end of that window, and not of the default.
// The window is short and the deadline is half the default one, so a slow machine
// has 450 ms to spare, and a hub on the default window misses the deadline by
// 500 ms.
func TestNewWiresTheConfiguredEventCoalesceWindow(t *testing.T) {
	const window = 50 * time.Millisecond
	cfg := config.Default()
	cfg.Cluster.Standalone.Ephemeral = true
	cfg.Auth.JWTSecret = strings.Repeat("event-coalesce-test-secret", 2)
	cfg.Events.Coalesce = window
	a, err := New(context.Background(), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	pulses := a.eventHub.Subscribe(t.Context())

	if n := burst(a.eventHub, pulses); n != 1 {
		t.Fatalf("%d of 5 pulses delivered at once, want only the first", n)
	}
	<-pulses
	select {
	case <-pulses:
	case <-time.After(event.DefaultCoalesce / 2):
		t.Fatalf("no trailing delivery within %v, the window is %v", event.DefaultCoalesce/2, window)
	}
}
