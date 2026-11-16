package interceptor

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
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

	// ForwardedKey marks a call that a follower forwarded to the leader: a call that
	// carries the key is marked, whatever its value, an empty one included. A
	// follower that gets a marked call does not forward it again, so a call crosses
	// one node at most: neither a stale view of the leader nor replicas that serve
	// gRPC on different ports can send it round in circles.
	ForwardedKey = "x-podium-forwarded"
)

// errForwarderClosed fails the forwards of a Forwarder that Close closed.
var errForwarderClosed = errors.New("forwarder closed")

// errLeaderUnreachable is the answer to a forward that the leader never answered. forward returns this very
// value, which UnaryLeader tells from an Unavailable that the leader sent.
var errLeaderUnreachable = status.Error(codes.Unavailable, "leader unreachable")

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

	// Refused, when set, is called once for each write that UnaryLeader answers itself because it cannot
	// send it to a leader: "not the leader", and "leader unreachable". A write that it forwards, and the
	// leader's answer to it, are not refusals, even when that answer is an Unavailable.
	Refused func()

	// Forwarded, when set, is called once for each write that UnaryLeader sends to the leader, as it sends it,
	// whatever the leader answers: a leader it cannot reach included, which Refused hears as well. A write sent
	// again after a handover counts once.
	Forwarded func()

	// HandoverWait bounds how long UnaryLeader waits, once the leader never answered a forward, for this node's
	// view of the leader to move (cluster.AwaitMove): to this node, which then serves the write itself, or to
	// another leader, to which it forwards the write once more. A view that has not moved by then leaves "leader
	// unreachable". Zero keeps cluster.HandoverWait; less than zero looks once and waits for nothing.
	HandoverWait time.Duration
}

// UnaryLeader routes the write RPCs that reach a follower, as the HTTP Leader middleware routes writes:
//   - Reads, and writes on the leader: pass through.
//   - A write that LeaderOptions.Forwarder forwards, on a follower that knows the leader's gRPC address: sent there
//     (see Forwarder), and the leader's answer, an error included, is the answer, with the x-leader hint of an
//     Unavailable that carries one. A forward the leader never answers, as at a handover, waits for this node's
//     view of the leader to move, up to LeaderOptions.HandoverWait, and is served here once this node leads, or
//     sent to the next leader once; a view that does not move leaves codes.Unavailable "leader unreachable" with
//     the x-leader hint of the leader it tried.
//   - Any other write on a follower, one that another node forwarded included (see ForwardedKey):
//     codes.Unavailable "not the leader" with the x-leader hint.
//
// The x-leader hint is the leader's gRPC address, host:port ([cluster.LeaderAddr] with LeaderOptions.Port), in the
// header metadata. It is not sent when that address cannot be derived: no leader is known, or the port is 0. A call
// it passes on carries, in its context, how to ask where the leader is, for a handler whose call meets a leadership
// error all the same (HintLeader).
func UnaryLeader(leadership cluster.Leadership, opts LeaderOptions) grpc.UnaryServerInterceptor {
	isWrite := opts.IsWrite
	if isWrite == nil {
		isWrite = allWrites
	}
	refused := opts.Refused
	if refused == nil {
		refused = func() {}
	}
	forwarded := opts.Forwarded
	if forwarded == nil {
		forwarded = func() {}
	}
	leaderOf := func() string {
		if leadership.AmLeader() {
			return ""
		}
		return cluster.LeaderAddr(leadership.CurrentLeader(), opts.Port)
	}

	handoverWait := opts.HandoverWait
	if handoverWait == 0 {
		handoverWait = cluster.HandoverWait
	}

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !isWrite(info.FullMethod) || leadership.AmLeader() {
			return handler(context.WithValue(ctx, leaderKey{}, leaderOf), req)
		}
		raft := leadership.CurrentLeader()
		leader := cluster.LeaderAddr(raft, opts.Port)
		if leader != "" && opts.Forwarder.forwards(info.FullMethod) && !forwardedHere(ctx) {
			forwarded()
			resp, err := opts.Forwarder.forward(ctx, leader, info.FullMethod, req)
			if err == errLeaderUnreachable {
				// A handover, most likely: the leader that left takes no call. The methods a Forwarder forwards
				// are safe to repeat, so the write goes where the view of the leader moves, whatever the leader
				// that left did with it.
				leading, moved := cluster.AwaitMove(ctx, leadership, raft, handoverWait)
				switch next := cluster.LeaderAddr(moved, opts.Port); {
				case leading:
					transportctx.SetError(ctx, "")
					return handler(context.WithValue(ctx, leaderKey{}, leaderOf), req)
				case next != "":
					transportctx.SetError(ctx, "")
					leader = next
					resp, err = opts.Forwarder.forward(ctx, next, info.FullMethod, req)
				}
			}
			if err == errLeaderUnreachable {
				refused()
				// The hint without the not-leader mark: a leader this node cannot reach is a failure, logged at
				// ERROR, as the HTTP forward logs its 502.
				setLeaderHint(ctx, leader)
			}
			return resp, err
		}
		refused()
		hintNotLeader(ctx, leader)
		return nil, status.Error(codes.Unavailable, "not the leader")
	}
}

func allWrites(string) bool { return true }

// leaderKey is the context key of the function, which UnaryLeader puts in the context of a call it passes on, that
// gives the leader's gRPC address as this node knows it when it is called: empty while this node leads or knows no
// leader.
type leaderKey struct{}

// HintLeader answers, for a handler, a call that met a leadership error after UnaryLeader passed it on: on a leader
// that lost its leadership while it served the call, or on a deposed leader whose check of its leadership failed. It
// sets the x-leader hint to the leader's gRPC address as this node knows it then, and marks the call as a routing
// answer, which UnaryLogger logs at WARN, as it logs a write UnaryLeader refuses. The handler answers the call
// Unavailable. Without a leader known, or for a call that UnaryLeader did not pass on, it sets nothing, and the call
// stays a failure, at ERROR, as an Unavailable without the hint is.
func HintLeader(ctx context.Context) {
	leader, ok := ctx.Value(leaderKey{}).(func() string)
	if !ok {
		return
	}
	hintNotLeader(ctx, leader())
}

// hintKey is the context key of a hintSlot.
type hintKey struct{}

// hintSlot records that a call answered "not the leader" with the x-leader hint, a routing condition. UnaryLogger
// installs it, outside the interceptors that route writes, which mark it, and reads it back once the call returns.
type hintSlot struct{ notLeader atomic.Bool }

// withHintSlot installs an empty hintSlot in ctx.
func withHintSlot(ctx context.Context) (context.Context, *hintSlot) {
	slot := &hintSlot{}
	return context.WithValue(ctx, hintKey{}, slot), slot
}

// setLeaderHint sets the x-leader hint to leader, the leader's gRPC address, unless it is empty, and reports
// whether it did.
func setLeaderHint(ctx context.Context, leader string) bool {
	return leader != "" && grpc.SetHeader(ctx, metadata.Pairs(LeaderKey, leader)) == nil
}

// hintNotLeader sets the x-leader hint of a write that a follower does not serve, and marks the hintSlot of ctx
// once the hint is set: the answer is a routing condition, which UnaryLogger logs at WARN. An unreachable leader
// sets the hint without the mark, and stays an ERROR.
func hintNotLeader(ctx context.Context, leader string) {
	if !setLeaderHint(ctx, leader) {
		return
	}
	if slot, ok := ctx.Value(hintKey{}).(*hintSlot); ok {
		slot.notLeader.Store(true)
	}
}

// forwardedHere reports whether another node forwarded the call ctx carries here, see ForwardedKey.
func forwardedHere(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	return len(md.Get(ForwardedKey)) > 0
}

// ForwardFunc sends req, the request of a forwarded call, to the leader over conn with opts, and returns the
// leader's response: the method of the generated client, such as DiscoverServiceClient.Sync.
type ForwardFunc func(ctx context.Context, conn grpc.ClientConnInterface, req any, opts ...grpc.CallOption) (any, error)

// Forwarder sends the writes of a follower to the leader over one client connection, which it keeps while the
// leader keeps its address, and replaces when the leader moves. Close closes it. The methods it forwards must be
// safe to repeat, as a Sync is: UnaryLeader sends a write the leader never answered to the next leader after a
// handover, and the leader that left may have run it.
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
// reached, is errLeaderUnreachable, and the error slot says why; the caller sets the x-leader hint once it gives
// up on the call. One whose caller gave up is the caller's own Canceled or DeadlineExceeded.
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
				hintNotLeader(ctx, hint[0])
			}
			return resp, callErr
		}
		err = callErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, status.FromContextError(ctxErr).Err()
	}
	transportctx.SetError(ctx, "forward to the leader at "+leader+": "+status.Convert(err).Message())
	return nil, errLeaderUnreachable
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
	// Straight to the leader, never through the proxy that HTTPS_PROXY names: grpc-go would take it from the
	// environment, and the leader would then see the proxy as the peer, which it trusts for no forwarding
	// metadata, and every forwarded agent as one client of its limiter. A connect, handshakes included, that
	// has not completed within cluster.ForwardDialTimeout fails the calls that wait on it, where grpc-go would
	// wait 20 seconds for a leader whose pod is gone.
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(f.creds), grpc.WithNoProxy(),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.DefaultConfig, MinConnectTimeout: cluster.ForwardDialTimeout}))
	if err != nil {
		return nil, err
	}
	if f.client != nil {
		// The former leader's. Closing it ends the calls still in flight on it: each answers its caller
		// Unavailable "leader unreachable", when the former leader had not begun to answer, or Canceled, the
		// code grpc-go gives a call cut on a closing connection, when it had. That is transient, as this node now
		// knows another leader: a client retries an Unavailable as any other, and a Canceled call as it retries
		// one it gave up itself, and an agent's next sync goes to the new leader in either case; a Sync is safe
		// to repeat.
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
