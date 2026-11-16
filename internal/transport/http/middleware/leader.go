package middleware

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// defaultForwardTimeout bounds a single proxied writing to the leader when LeaderOptions.ForwardTimeout is left unset.
const defaultForwardTimeout = 30 * time.Second

// ForwardedHeader marks a write that a follower forwarded to the leader, as the gRPC x-podium-forwarded metadata
// marks a call: a write that carries the header is marked, whatever its value, an empty one included. A follower
// never forwards a marked write again, so a write crosses one node at most: neither a stale view of the leader nor
// replicas that serve this listener on different ports can send it round. A client that sets the mark itself only
// keeps its own write from being forwarded.
const ForwardedHeader = "X-Podium-Forwarded"

// defaultWriteMethods: mutations require leader authority.
var defaultWriteMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// LeaderOptions tunes the Leader middleware.
type LeaderOptions struct {
	// IsWrite overrides the default write detection.
	IsWrite func(*http.Request) bool

	// ForwardPort is the TCP port on which the OTHER replicas serve this HTTP endpoint.
	//
	// When non-zero, the middleware transparently reverse-proxies write requests to the current leader's host at this port.
	// When zero, the middleware returns 503 and expects the client (or ingress with retry) to recover. It cannot tell where
	// the leader serves this endpoint, so it sends no X-Leader.
	//
	// Leadership.CurrentLeader() returns the Raft TCP address (host:raftPort)
	// Take only the host and substitute ForwardPort; the same middleware works for main API and discovery.
	ForwardPort int

	// ForwardTimeout caps a single proxied write to the leader (time to first response header).
	// Without it a hung leader would pin the follower's request goroutine indefinitely.
	ForwardTimeout time.Duration

	// TargetTLS reports whether the leader's target listener serves TLS. The proxy
	// scheme must follow the TARGET listener, not how the inbound request arrived —
	// otherwise a plaintext request forwarded to a TLS listener (or vice versa) fails
	// the handshake ("client sent an HTTP request to an HTTPS server").
	TargetTLS bool

	// TLSClientConfig is the client side of a forward to a TLS target: the CAs
	// that verify the leader's certificate, for its ServerName, or the leader's
	// host without one, and the client certificate presented when the leader
	// asks for one. Nil verifies the leader against the system roots, for its
	// host, with no client certificate.
	TLSClientConfig *tls.Config

	// HandoverWait bounds how long a follower waits, once a forward failed to
	// connect, which sent the leader nothing of the write, for its view of the
	// leader to move (cluster.AwaitMove): to this node, which then serves the
	// write itself, or to another leader, to which it sends the write once more.
	// A view that has not moved by then leaves the 502. Zero keeps
	// cluster.HandoverWait; less than zero looks once and waits for nothing.
	HandoverWait time.Duration

	// Refused, when set, is called once for each write that this node answers
	// itself with a 502 or 503 because it cannot send it to a leader: no leader
	// is known, the request was forwarded here already, the leader's address is
	// unusable, or the leader cannot be reached, after a wait for a handover. A
	// write that it proxies, and the leader's answer to it, are not refusals,
	// nor is a write that it serves itself after a handover. It runs on the
	// request's goroutine.
	Refused func()

	// Forwarded, when set, is called once for each write that this node sends to
	// the leader, as it sends it, whatever the leader answers: a leader it cannot
	// reach included, which Refused hears as well. A write sent again after a
	// handover counts once. It runs on the request's goroutine.
	Forwarded func()

	// Log, when set, logs what becomes of a write besides a forward the leader
	// answers: each write this node refuses, as Refused hears it, at WARN for a
	// 503 that names the leader, a routing answer, and at ERROR for any other,
	// as the access log ranks them, with the reason; and each write it sends to
	// the next leader, or serves itself, after a handover, at INFO. The reason of
	// a refusal goes to the request's error slot too (transportctx.SetError), so
	// a listener whose access log wraps this middleware logs it there and leaves
	// Log unset. The zero Logger logs nothing.
	Log zerolog.Logger
}

// Leader routes write requests to the cluster leader.
//
//   - Reads: pass through.
//   - Writes on leader: pass through, marked or not.
//   - Writes on the follower with ForwardPort == 0: 503, without X-Leader.
//   - Writes on the follower that carry ForwardedHeader, which another node forwarded already: 503, with the
//     leader's HTTP address in X-Leader. They are not forwarded again.
//   - Writes on follower with ForwardPort > 0: transparent reverse-proxy to http://<leader-host>:<ForwardPort>,
//     or https:// with TargetTLS, verified and authenticated with TLSClientConfig, marked with ForwardedHeader. A
//     leader it cannot reach is a 502 with the leader's HTTP address in X-Leader. The leader's answer goes back
//     without its CORS headers (see dropCORS).
//   - A forward that fails to connect sent nothing of the write, as at a handover, when the leader that left
//     refuses the connection, or its pod is gone (cluster.ForwardDialTimeout): the follower waits for its view of
//     the leader to move, up to LeaderOptions.HandoverWait, and serves the write itself once it leads, or sends it
//     to the next leader, once; the 502 stays for a view that does not move. A forward that was ever handed a
//     connection is never sent again, whatever happened to it: the leader may have run the write.
//   - A write that a follower could forward while it knows no leader, as between a release and the takeover,
//     waits for a leader the same way, and the 503 without X-Leader stays for none known by then.
//
// A request that passes through carries, in its context, where the leader serves this listener
// (httpctx.Leader, see leaderAddress), so that a write that reaches a follower anyway, such as a
// refresh with rotation on a read, answers 503 with that address in X-Leader (response.FromError).
// X-Leader always has the one meaning documented in internal/transport/README.md, and every answer sets
// it through response.SetLeader.
//
// On the memory backend AmLeader is always true, so the middleware adds essentially zero overhead:
// that context value, which says nothing.
func Leader(leadership cluster.Leadership, opts LeaderOptions) func(http.Handler) http.Handler {
	isWrite := opts.IsWrite
	if isWrite == nil {
		isWrite = defaultIsWrite
	}

	timeout := opts.ForwardTimeout
	if timeout <= 0 {
		timeout = defaultForwardTimeout
	}
	handoverWait := opts.HandoverWait
	if handoverWait == 0 {
		handoverWait = cluster.HandoverWait
	}
	transport := markForwarded{next: &http.Transport{
		DialContext: (&net.Dialer{Timeout: cluster.ForwardDialTimeout}).DialContext,
		// Straight to the leader, never through the proxy that HTTP_PROXY or
		// HTTPS_PROXY names: the leader would see the proxy as the peer, which it
		// trusts for no forwarding header, and every forwarded client as one
		// client of its login limiter.
		Proxy:                 nil,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		// A leader that takes the connection and stalls the handshake is gone too, as one that takes none is.
		TLSHandshakeTimeout: cluster.ForwardDialTimeout,
		// A clone: other clients, such as the proxy pool, share the configuration.
		TLSClientConfig: opts.TLSClientConfig.Clone(),
	}}

	refused := opts.Refused
	if refused == nil {
		refused = func() {}
	}
	forwarded := opts.Forwarded
	if forwarded == nil {
		forwarded = func() {}
	}
	leader := func() string { return leaderAddress(leadership, opts.ForwardPort) }
	// refuse answers a write that this node cannot send to a leader with code and answer, and X-Leader when
	// leaderAddr, the leader's HTTP address, is known; reason says why, to the error slot and to Log.
	refuse := func(w http.ResponseWriter, r *http.Request, code int, leaderAddr, answer, reason string) {
		refused()
		transportctx.SetError(r.Context(), reason)
		evt := opts.Log.Error()
		if code == http.StatusServiceUnavailable && leaderAddr != "" {
			evt = opts.Log.Warn()
		}
		evt.Str("method", r.Method).Str("path", logPath(r.URL.Path)).Int("status", code).
			Str("leader", leaderAddr).Str("error", reason).Msg("write not forwarded to the leader")
		response.SetLeader(w, leaderAddr)
		http.Error(w, answer, code)
	}
	return func(next http.Handler) http.Handler {
		serve := func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(httpctx.WithLeader(r.Context(), leader)))
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWrite(r) || leadership.AmLeader() {
				serve(w, r)
				return
			}
			leaderAddr := leadership.CurrentLeader()
			if leaderAddr == "" && opts.ForwardPort != 0 && len(r.Header.Values(ForwardedHeader)) == 0 {
				// No leader known, as between a leader's release and the takeover: the write went nowhere, and
				// waits for a leader as it does after a forward that failed to connect.
				leading, known := cluster.AwaitMove(r.Context(), leadership, "", handoverWait)
				if leading {
					opts.Log.Info().Str("method", r.Method).Str("path", logPath(r.URL.Path)).
						Msg("write served here once this node led")
					serve(w, r)
					return
				}
				leaderAddr = known
			}
			if leaderAddr == "" {
				refuse(w, r, http.StatusServiceUnavailable, "", "cluster: no leader", "no leader is known")
				return
			}
			if opts.ForwardPort == 0 {
				// No port to forward to, and none to tell the client: the leader's raft address is no HTTP address.
				refuse(w, r, http.StatusServiceUnavailable, "", "cluster: not leader", "no port to forward to")
				return
			}
			if len(r.Header.Values(ForwardedHeader)) > 0 {
				// Forwarded once already, to a node that leads no more, or never did on this port.
				refuse(w, r, http.StatusServiceUnavailable, leaderHTTPAddr(leaderAddr, opts.ForwardPort), "cluster: not leader",
					"forwarded here already by another node, which took this node for the leader")
				return
			}

			target, err := buildTarget(leaderAddr, opts.ForwardPort, opts.TargetTLS)
			if err != nil {
				refuse(w, r, http.StatusBadGateway, "", "cluster: bad leader addr", "the leader's address gives no address to forward to: "+err.Error())
				return
			}
			// The body stays open, and unread, through a forward that fails to connect, for the next leader or this
			// node; it closes once the write is answered, as the reverse proxy would close it. The trace hears
			// whether the transport ever handed the forward a connection, which may have carried the write.
			body := &unsentBody{ReadCloser: r.Body}
			defer func() { _ = r.Body.Close() }()
			var connected atomic.Bool
			traced := r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
				GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
			}))
			// The transport marks what the proxy sends (markForwarded).
			forwarded()
			err = proxyWrite(w, traced, body, target, transport)
			if err != nil && unsent(r, body, connected.Load(), err) {
				leading, moved := cluster.AwaitMove(r.Context(), leadership, leaderAddr, handoverWait)
				switch {
				case leading:
					opts.Log.Info().Str("method", r.Method).Str("path", logPath(r.URL.Path)).
						Str("leader", leaderHTTPAddr(leaderAddr, opts.ForwardPort)).Str("error", err.Error()).
						Msg("write served here after a handover")
					serve(w, r)
					return
				case moved != "":
					if nextTarget, buildErr := buildTarget(moved, opts.ForwardPort, opts.TargetTLS); buildErr == nil {
						opts.Log.Info().Str("method", r.Method).Str("path", logPath(r.URL.Path)).
							Str("leader", leaderHTTPAddr(leaderAddr, opts.ForwardPort)).Str("next_leader", nextTarget.Host).
							Str("error", err.Error()).Msg("write sent to the next leader after a handover")
						leaderAddr = moved
						err = proxyWrite(w, traced, body, nextTarget, transport)
					}
				}
			}
			if err != nil {
				to := leaderHTTPAddr(leaderAddr, opts.ForwardPort)
				refuse(w, r, http.StatusBadGateway, to, "cluster: leader unreachable", "forward to the leader at "+to+": "+err.Error())
			}
		})
	}
}

// proxyWrite sends r, with body, to target through transport, and writes the leader's answer to w. It returns
// the error of a forward that got no answer, and then writes nothing to w: the caller answers.
func proxyWrite(w http.ResponseWriter, r *http.Request, body io.ReadCloser, target *url.URL, transport http.RoundTripper) error {
	var failed error
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ModifyResponse = dropCORS
	proxy.ErrorHandler = func(_ http.ResponseWriter, _ *http.Request, err error) { failed = err }
	out := *r
	out.Body = body
	proxy.ServeHTTP(w, &out)
	return failed
}

// unsentBody is the body of a write a follower forwards: it counts what the transport read of it, and stays open
// when the transport closes it, as a transport closes the body of a request it could not send, so that the write
// can go to the next leader, or to this node's handler, after a forward that failed to connect.
type unsentBody struct {
	io.ReadCloser
	read atomic.Int64
}

func (b *unsentBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// Close leaves the body open: the handler closes it once the write is answered.
func (*unsentBody) Close() error { return nil }

// unsent reports whether err, the error of a forward of r, says that the leader got nothing of the write: the dial
// failed, the transport never handed the forward a connection (connected), so nothing was sent, not a byte of body
// was read, and the client still waits. A connection handed out may have carried the write, even when the
// transport then retried on a new dial that failed, as it does for a replayable request on an idle connection the
// leader closed. Any other failure, a handshake that a leader's certificate fails included, is final: a write that
// may have reached the leader is never sent again, and a leader that the follower cannot verify is no handover.
func unsent(r *http.Request, body *unsentBody, connected bool, err error) bool {
	var op *net.OpError
	return r.Context().Err() == nil && !connected && body.read.Load() == 0 && errors.As(err, &op) && op.Op == "dial"
}

// markForwarded is the transport of the forwards to the leader: it sets ForwardedHeader on every request it sends,
// after the reverse proxy removed the hop-by-hop headers, as httputil.ReverseProxy does once its Director ran. A
// client that names the header in its Connection header would make the proxy remove a mark set before, so that a
// write would cross two nodes. RoundTrip changes no request it gets: it sends a copy.
type markForwarded struct{ next http.RoundTripper }

func (t markForwarded) RoundTrip(r *http.Request) (*http.Response, error) {
	marked := r.Clone(r.Context())
	marked.Header.Set(ForwardedHeader, "1")
	return t.next.RoundTrip(marked)
}

// leaderAddress is where the leader serves this listener, as a follower knows it (see leaderHTTPAddr). It is
// empty when this node is the leader or no leader is known.
func leaderAddress(leadership cluster.Leadership, port int) string {
	if leadership.AmLeader() {
		return ""
	}
	return leaderHTTPAddr(leadership.CurrentLeader(), port)
}

// leaderHTTPAddr is the HTTP address, host:port, at which the leader whose raft address is leaderAddr serves
// this listener: its host, with port, which every replica is assumed to serve the listener on
// ([cluster.LeaderAddr], the derivation the gRPC x-leader hint shares). It is the forward target and X-Leader
// alike. It is empty without a leader or a port: the address cannot be derived, and no X-Leader goes out.
func leaderHTTPAddr(leaderAddr string, port int) string {
	return cluster.LeaderAddr(leaderAddr, port)
}

// dropCORS removes the CORS headers from the leader's answer to a forwarded write: every Access-Control-* header,
// and the Origin token of Vary, whose other tokens stay. The follower's own CORS middleware, which runs before the
// forward, owns these headers and has set them already, and the reverse proxy adds the leader's headers to the
// follower's: a browser refuses an answer that names its origin twice in Access-Control-Allow-Origin.
func dropCORS(res *http.Response) error {
	for name := range res.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
			delete(res.Header, name)
		}
	}
	var vary []string
	for _, line := range res.Header.Values("Vary") {
		var kept []string
		for _, token := range strings.Split(line, ",") {
			if token = strings.TrimSpace(token); token != "" && !strings.EqualFold(token, "Origin") {
				kept = append(kept, token)
			}
		}
		if len(kept) > 0 {
			vary = append(vary, strings.Join(kept, ", "))
		}
	}
	res.Header.Del("Vary")
	for _, line := range vary {
		res.Header.Add("Vary", line)
	}
	return nil
}

func buildTarget(leaderAddr string, port int, tls bool) (*url.URL, error) {
	addr := leaderHTTPAddr(leaderAddr, port)
	if addr == "" {
		return nil, errors.New("no leader address or port to forward to")
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	u, err := url.Parse(scheme + "://" + addr)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func defaultIsWrite(r *http.Request) bool {
	_, ok := defaultWriteMethods[r.Method]
	return ok
}
