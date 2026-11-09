package app

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	discoverv1 "github.com/soltiHQ/control-plane/api/gen/solti/discover/v1"
	"github.com/soltiHQ/control-plane/internal/cluster"
)

// The reasons /readyz gives for a node that is not ready.
const (
	reasonStarting     = "starting"
	reasonStorage      = "storage failed"
	reasonNoLeader     = "no raft leader"
	reasonShuttingDown = "shutting down"
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

// newReadiness returns the readiness of a node over store and leadership: a
// durable store can fail, and a raft node, which is the one with members, needs
// to know a leader.
func newReadiness(store any, leadership cluster.Leadership) *readiness {
	r := &readiness{}
	if f, ok := store.(interface{ Failure() error }); ok {
		r.failure = f.Failure
	}
	if _, raft := leadership.(cluster.Membership); raft {
		r.leader = leadership.CurrentLeader
	}
	return r
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

// grpcHealth is the grpc.health.v1 service of the gRPC listener: the overall
// service "" and the discovery service are SERVING while the node is ready.
type grpcHealth struct {
	*health.Server
	ready *readiness
}

// newGRPCHealth returns the health service over ready, with the current state.
func newGRPCHealth(ready *readiness) *grpcHealth {
	g := &grpcHealth{Server: health.NewServer(), ready: ready}
	g.sync()
	return g
}

// sync sets the status of each service from the readiness.
func (g *grpcHealth) sync() {
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
