package middleware

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
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
	// that verify the leader's certificate, for the leader's host, and the
	// client certificate presented when the leader asks for one. Nil verifies
	// the leader against the system roots, with no client certificate.
	TLSClientConfig *tls.Config
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
//
// A request that passes through carries, in its context, where the leader serves this listener
// (httpctx.Leader, see leaderAddress), so that a write that reaches a follower anyway, such as a
// refresh with rotation on a read, answers 503 with that address in X-Leader (response.FromError).
// X-Leader always has the one meaning documented in internal/transport/README.md, and every answer sets
// it through response.SetLeader.
//
// In a standalone deployment AmLeader is always true, so the middleware adds essentially zero overhead:
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
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		Proxy:                 http.ProxyFromEnvironment,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		// A clone: other clients, such as the proxy pool, share the configuration.
		TLSClientConfig: opts.TLSClientConfig.Clone(),
	}

	leader := func() string { return leaderAddress(leadership, opts.ForwardPort) }
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWrite(r) || leadership.AmLeader() {
				next.ServeHTTP(w, r.WithContext(httpctx.WithLeader(r.Context(), leader)))
				return
			}
			leaderAddr := leadership.CurrentLeader()
			if leaderAddr == "" {
				http.Error(w, "cluster: no leader", http.StatusServiceUnavailable)
				return
			}
			if opts.ForwardPort == 0 {
				// No port to forward to, and none to tell the client: the leader's raft address is no HTTP address.
				http.Error(w, "cluster: not leader", http.StatusServiceUnavailable)
				return
			}
			if len(r.Header.Values(ForwardedHeader)) > 0 {
				// Forwarded once already, to a node that leads no more, or never did on this port.
				response.SetLeader(w, leaderHTTPAddr(leaderAddr, opts.ForwardPort))
				http.Error(w, "cluster: not leader", http.StatusServiceUnavailable)
				return
			}

			target, err := buildTarget(leaderAddr, opts.ForwardPort, opts.TargetTLS)
			if err != nil {
				http.Error(w, "cluster: bad leader addr", http.StatusBadGateway)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = transport
			proxy.ModifyResponse = dropCORS
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
				response.SetLeader(w, leaderHTTPAddr(leaderAddr, opts.ForwardPort))
				http.Error(w, "cluster: leader unreachable", http.StatusBadGateway)
			}
			// The mark goes on a copy, which the proxy forwards: a handler must not change the request it gets.
			marked := r.Clone(r.Context())
			marked.Header.Set(ForwardedHeader, "1")
			proxy.ServeHTTP(w, marked)
		})
	}
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
