package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
)

// The reasons /readyz gives for a node that is not ready.
const (
	reasonStarting     = "starting"
	reasonStorage      = "storage failed"
	reasonNoLeader     = "no raft leader"
	reasonShuttingDown = "shutting down"
	// reasonNotLeader is what /leaderz says of a node that serves but does not lead.
	reasonNotLeader = "not leader"
)

// healthSyncInterval is how often the gRPC health service re-reads readiness for
// the clients that Watch it. Check reads it anew at each call.
const healthSyncInterval = time.Second

// readiness is whether this node is ready to serve: what /readyz and the gRPC
// health service report.
type readiness struct {
	// bound is set once every listener is bound; the store is open before New
	// returns, so the start-up has finished then.
	bound atomic.Bool
	// shuttingDown is set at the first shutdown signal and never cleared.
	shuttingDown atomic.Bool
	// failure returns why the store stopped serving, nil while it serves; nil for
	// a store that cannot fail.
	failure func() error
	// leader returns the address of the leader this node knows, "" for none; nil
	// when the node needs no leader, which is standalone.
	leader func() string
}

// probeHooks are what buildCluster tells of the store it built, where its type
// is known: a durable store can fail, and a raft node needs to know a leader. A
// hook that the backend does not have stays nil.
type probeHooks struct {
	failure func() error
	leader  func() string
}

// withProbes makes buildCluster fill p with the hooks of the backend it builds.
func withProbes(p *probeHooks) clusterOption {
	return func(o *clusterOptions) { o.probes = p }
}

// newReadiness returns the readiness of a node over the hooks of its store.
func newReadiness(h probeHooks) *readiness {
	return &readiness{failure: h.failure, leader: h.leader}
}

// reason returns "" while the node is ready, and otherwise why it is not, one of
// the reasons above.
func (r *readiness) reason() string {
	switch {
	case r.shuttingDown.Load():
		return reasonShuttingDown
	case !r.bound.Load():
		return reasonStarting
	case r.failure != nil && r.failure() != nil:
		return reasonStorage
	case r.leader != nil && r.leader() == "":
		return reasonNoLeader
	}
	return ""
}

// leaderReason returns "" while this node is the leader that /leaderz routes to,
// and otherwise why it is not. A raft node is the leader once start-up has
// finished and it leads, whatever the store does and also while the shutdown
// delay runs, when it still serves; a standalone node, which has no leader to
// find, is the leader while it is ready.
func (r *readiness) leaderReason(amLeader func() bool) string {
	switch {
	case r.leader == nil:
		return r.reason()
	case !r.bound.Load():
		return reasonStarting
	case !amLeader():
		return reasonNotLeader
	}
	return ""
}

// grpcHealth is the grpc.health.v1 service of the gRPC listener: the overall
// service "" and the discovery service are SERVING while the node is ready.
type grpcHealth struct {
	*health.Server
	ready *readiness
	// mu holds the read of the readiness and the writes of the statuses together,
	// so that a stale sync cannot write SERVING after one that saw the shutdown.
	mu sync.Mutex
	// done closes when the gRPC listener starts to stop; it ends the Watch streams.
	done     chan struct{}
	doneOnce sync.Once
}

// newGRPCHealth returns the health service over ready, with the current state.
func newGRPCHealth(ready *readiness) *grpcHealth {
	g := &grpcHealth{Server: health.NewServer(), ready: ready, done: make(chan struct{})}
	g.sync()
	return g
}

// sync sets the status of each service from the readiness.
func (g *grpcHealth) sync() {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := healthpb.HealthCheckResponse_SERVING
	if g.ready.reason() != "" {
		st = healthpb.HealthCheckResponse_NOT_SERVING
	}
	for _, service := range []string{"", discoverv1.DiscoverService_ServiceDesc.ServiceName} {
		g.SetServingStatus(service, st)
	}
}

// Check answers with the status of the node now.
func (g *grpcHealth) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	g.sync()
	return g.Server.Check(ctx, req)
}

// end ends the Watch streams, and every one opened after it. The gRPC listener
// calls it as it starts to stop: GracefulStop waits for streams, and a Watch
// ends only when its client leaves.
func (g *grpcHealth) end() { g.doneOnce.Do(func() { close(g.done) }) }

// Watch streams the status of the service until the client leaves or end is called.
func (g *grpcHealth) Watch(req *healthpb.HealthCheckRequest, stream healthpb.Health_WatchServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	go func() {
		select {
		case <-g.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return g.Server.Watch(req, watchStream{Health_WatchServer: stream, ctx: ctx})
}

// watchStream is a Watch stream whose context ends with the health service's.
type watchStream struct {
	healthpb.Health_WatchServer
	ctx context.Context
}

func (w watchStream) Context() context.Context { return w.ctx }

// run keeps the status that Watch streams current until ctx ends.
func (g *grpcHealth) run(ctx context.Context) {
	tick := time.NewTicker(healthSyncInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			g.sync()
		case <-ctx.Done():
			return
		}
	}
}
