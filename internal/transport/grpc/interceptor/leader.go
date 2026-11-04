package interceptor

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/clientip"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

const (
	// LeaderKey is the header metadata key of the x-leader hint: the leader's gRPC
	// address, host:port.
	LeaderKey = "x-leader"

	// ForwardedKey marks a call that a follower forwarded to the leader. A follower
	// that gets a marked call does not forward it again, so a call crosses one node
	// at most: neither a stale view of the leader nor replicas that serve gRPC on
	// different ports can send it round in circles.
	ForwardedKey = "x-podium-forwarded"
)

// errForwarderClosed fails the forwards of a Forwarder that Close closed.
var errForwarderClosed = errors.New("forwarder closed")

// LeaderOptions tunes the [UnaryLeader] and [StreamLeader] interceptors.
type LeaderOptions struct {
	// IsWrite decides whether an RPC must run on the leader.
	// The argument is the full method name, e.g. "/solti.discover.v1.DiscoverService/Sync".
	// If nil, UnaryLeader treats every call as a write, the safe default, and StreamLeader none.
	IsWrite func(fullMethod string) bool

	// Port is the port of this gRPC listener, which every replica is assumed to serve it on (the same-port
	// assumption): the leader serves it at the host of its raft address and Port ([cluster.LeaderAddr]). That
	// address is the x-leader hint and the one Forwarder sends a call to. Zero leaves it unknown: no hint goes
	// out and nothing is forwarded.
	Port int

	// Forwarder, when set, sends the writes it has a ForwardFunc for to the leader instead of refusing them.
	Forwarder *Forwarder
}

// UnaryLeader routes the write RPCs that reach a follower, as the HTTP Leader middleware routes writes:
//   - Reads, and writes on the leader: pass through.
//   - A write that LeaderOptions.Forwarder forwards, on a follower that knows the leader's gRPC address: sent there
//     (see Forwarder), and the leader's answer, an error included, is the answer, with the x-leader hint of an
//     Unavailable that carries one. A forward the leader never answers is codes.Unavailable "leader unreachable"
//     with the x-leader hint.
//   - Any other write on a follower, one that another node forwarded included (see ForwardedKey):
//     codes.Unavailable "not the leader" with the x-leader hint.
//
// The x-leader hint is the leader's gRPC address, host:port ([cluster.LeaderAddr] with LeaderOptions.Port), in the
// header metadata. It is not sent when that address cannot be derived: no leader is known, or the port is 0.
func UnaryLeader(leadership cluster.Leadership, opts LeaderOptions) grpc.UnaryServerInterceptor {
	isWrite := opts.IsWrite
	if isWrite == nil {
		isWrite = allWrites
	}

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !isWrite(info.FullMethod) || leadership.AmLeader() {
			return handler(ctx, req)
		}
		leader := cluster.LeaderAddr(leadership.CurrentLeader(), opts.Port)
		if leader != "" && opts.Forwarder.forwards(info.FullMethod) && !forwarded(ctx) {
			return opts.Forwarder.forward(ctx, leader, info.FullMethod, req)
		}
		setLeaderHint(ctx, leader)
		return nil, status.Error(codes.Unavailable, "not the leader")
	}
}

func allWrites(string) bool { return true }

// hintKey is the context key of a hintSlot.
type hintKey struct{}

// hintSlot records that a call answered with the x-leader hint. UnaryLogger installs it, outside the interceptors
// that route writes, which mark it, and reads it back once the call returns.
type hintSlot struct{ set atomic.Bool }

// withHintSlot installs an empty hintSlot in ctx.
func withHintSlot(ctx context.Context) (context.Context, *hintSlot) {
	slot := &hintSlot{}
	return context.WithValue(ctx, hintKey{}, slot), slot
}

// setLeaderHint sets the x-leader hint to leader, the leader's gRPC address, unless it is empty, and marks the
// hintSlot of ctx once the hint is set.
func setLeaderHint(ctx context.Context, leader string) {
	if leader == "" || grpc.SetHeader(ctx, metadata.Pairs(LeaderKey, leader)) != nil {
		return
	}
	if slot, ok := ctx.Value(hintKey{}).(*hintSlot); ok {
		slot.set.Store(true)
	}
}

// forwarded reports whether another node forwarded the call ctx carries, see ForwardedKey.
func forwarded(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	return len(md.Get(ForwardedKey)) > 0
}

// ForwardFunc sends req, the request of a forwarded call, to the leader over conn with opts, and returns the
// leader's response: the method of the generated client, such as DiscoverServiceClient.Sync.
type ForwardFunc func(ctx context.Context, conn grpc.ClientConnInterface, req any, opts ...grpc.CallOption) (any, error)

// Forwarder sends the writes of a follower to the leader over one client connection, which it keeps while the
// leader keeps its address, and replaces when the leader moves. Close closes it.
type Forwarder struct {
	creds credentials.TransportCredentials
	calls map[string]ForwardFunc // by full method name

	mu     sync.Mutex
	addr   string           // the leader's gRPC address that client is for
	client *grpc.ClientConn // nil before the first forward, and once closed
	closed bool
}

// NewForwarder returns the Forwarder of the methods of calls. targetTLS says whether the leader's gRPC listener
// serves TLS, as this node's does with tls.server: it then dials the leader over TLS with tlsConfig, the
// tls.client configuration, which verifies the leader's certificate for the host it dials, the leader's raft host,
// and presents tls.client's certificate when the leader asks for one, as the HTTP Leader middleware does. A nil
// tlsConfig verifies the leader against the system roots. Without targetTLS it dials plaintext.
func NewForwarder(targetTLS bool, tlsConfig *tls.Config, calls map[string]ForwardFunc) *Forwarder {
	creds := insecure.NewCredentials()
	if targetTLS {
		// NewTLS works on a copy, so the proxy pool keeps sharing tlsConfig untouched.
		creds = credentials.NewTLS(tlsConfig)
	}
	return &Forwarder{creds: creds, calls: calls}
}

// forwards reports whether f forwards method.
func (f *Forwarder) forwards(method string) bool {
	if f == nil {
		return false
	}
	_, ok := f.calls[method]
	return ok
}

// forward sends the call ctx carries to the leader at leader, its gRPC address, before the caller's own deadline,
// and returns the leader's answer, an error included. A call the leader never answered, as it could not be
// reached, is codes.Unavailable "leader unreachable" with the x-leader hint, and the error slot says why; one
// whose caller gave up is the caller's own Canceled or DeadlineExceeded.
func (f *Forwarder) forward(ctx context.Context, leader, method string, req any) (any, error) {
	conn, err := f.conn(leader)
	if err == nil {
		var header metadata.MD
		resp, callErr := f.calls[method](metadata.NewOutgoingContext(ctx, forwardMetadata(ctx)), conn, req, grpc.Header(&header))
		// The leader's interceptors send header metadata, its request ID, with every answer: a call that got
		// none never reached them.
		if callErr == nil || len(header) > 0 {
			if hint := header.Get(LeaderKey); status.Code(callErr) == codes.Unavailable && len(hint) > 0 {
				// A node that does not lead refused the forwarded call: its view of the leader goes on to
				// the caller, and the answer stays a routing answer.
				setLeaderHint(ctx, hint[0])
			}
			return resp, callErr
		}
		err = callErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, status.FromContextError(ctxErr).Err()
	}
	transportctx.SetError(ctx, "forward to the leader at "+leader+": "+status.Convert(err).Message())
	setLeaderHint(ctx, leader)
	return nil, status.Error(codes.Unavailable, "leader unreachable")
}

// conn returns the connection to the leader at addr: the one f keeps when it is for addr, or a new one in its
// place, which dials when the first call needs it.
func (f *Forwarder) conn(addr string) (*grpc.ClientConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errForwarderClosed
	}
	if f.client != nil && f.addr == addr {
		return f.client, nil
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(f.creds))
	if err != nil {
		return nil, err
	}
	if f.client != nil {
		// The former leader's: a call still on it ends, as that node leads no more.
		_ = f.client.Close()
	}
	f.addr, f.client = addr, conn
	return conn, nil
}

// Close closes the connection to the leader. A later forward fails as one that cannot reach the leader.
func (f *Forwarder) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.client == nil {
		return nil
	}
	err := f.client.Close()
	f.addr, f.client = "", nil
	return err
}

// forwardMetadata is the metadata of the call ctx carries as the leader gets it, and nothing else:
//   - authorization, unchanged: the only metadata the handlers read;
//   - the request ID, so that both nodes log the call under one;
//   - x-forwarded-for: its own values with the caller's peer appended, as an HTTP reverse proxy appends it to
//     X-Forwarded-For, which the leader believes only from a trusted peer;
//   - ForwardedKey.
func forwardMetadata(ctx context.Context) metadata.MD {
	in, _ := metadata.FromIncomingContext(ctx)
	out := metadata.Pairs(ForwardedKey, "1")
	if auth := in.Get("authorization"); len(auth) > 0 {
		out["authorization"] = slices.Clone(auth)
	}
	if rid, ok := transportctx.RequestID(ctx); ok {
		out.Set(transportctx.DefaultRequestIDHeader, rid)
	}
	client := peerIP(ctx)
	if prior := in.Get(clientip.ForwardedForKey); len(prior) > 0 {
		client = strings.Join(prior, ", ") + ", " + client
	}
	out.Set(clientip.ForwardedForKey, client)
	return out
}

// peerIP is the canonical IP of the caller's peer, or "unknown".
func peerIP(ctx context.Context) string {
	var addr string
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	return clientip.FromAddr(addr)
}
