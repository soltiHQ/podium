package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// maxAgentMessageBytes is the maximum gRPC message size (both send and
// receive) used for calls to agents. Must equal solti-api's MAX_REQUEST_BYTES
// (4 MiB) so both ends of the wire agree.
//
// The value matches grpc-go's own `MaxCallRecvMsgSize` default — we set it
// explicitly rather than inheriting the default because (a) the default can
// change between grpc-go versions, and (b) `MaxCallSendMsgSize` is NOT 4 MiB
// by default (it's `MaxInt32`), so capping sends here makes runaway
// allocations impossible. This is belt-and-suspenders but cheap.
//
// Sizing: a `Script`-mode task body is capped at 2 MiB in the model;
// base64 inflation × 4/3 + envelope overhead fits under 4 MiB with ~33% headroom.
const maxAgentMessageBytes = 4 * 1024 * 1024

// maxAgentHeaderListBytes is the largest header list, headers or trailers, that a
// gRPC client accepts from an agent: grpc-go counts the sizes of the names and
// values, and 32 bytes for each entry. The status message of a failed call travels
// in the trailers, so the limit bounds the error text an agent can send; the
// default of grpc-go is 16 MiB. A call whose answer carries a longer list fails
// with an error of the transport, and the status is lost.
const maxAgentHeaderListBytes = 64 << 10

// grpcEntry is a pooled connection and the target it dials.
type grpcEntry struct {
	target string
	conn   *grpc.ClientConn
}

// Pool manages shared outbound connections to agents.
//
// For HTTP it holds a single *http.Client whose Transport pools TCP connections.
// For gRPC it holds one *grpc.ClientConn for each agent, to the endpoint the agent
// advertises now: a Get for another endpoint closes the old connection, Forget
// closes it for an agent that is gone, so the pool grows with the agents and not
// with the endpoints they once advertised. The pool keeps no token: Get takes it
// on every call.
type Pool struct {
	mu     sync.RWMutex
	closed bool

	httpCli     *http.Client
	grpcConns   map[string]grpcEntry // by agent ID
	grpcCreds   credentials.TransportCredentials
	requireAuth bool
}

// NewPool creates a Pool with a configured HTTP transport.
//
// clientTLS configures outbound TLS to agents (CP-as-client). When nil the pool
// stays plaintext: HTTP uses a minimal TLS-1.2 config (system roots, only
// engaged for https:// endpoints) and gRPC dials insecurely — preserving the
// pre-TLS behavior.
//
// requireAuth mirrors agent_auth.require: when false, no stored token is sent.
func NewPool(clientTLS *tls.Config, requireAuth bool) *Pool {
	httpTLS := clientTLS
	if httpTLS == nil {
		httpTLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	creds := credentials.TransportCredentials(insecure.NewCredentials())
	if clientTLS != nil {
		creds = credentials.NewTLS(clientTLS)
	}

	return &Pool{
		httpCli: &http.Client{
			// A redirect would carry the request to an address discovery never validated.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSClientConfig:       httpTLS,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
			},
		},
		grpcConns:   make(map[string]grpcEntry),
		grpcCreds:   creds,
		requireAuth: requireAuth,
	}
}

// Get returns an AgentProxy for the agent bound to the given endpoint and
// (optional) bearer token, selecting the implementation by API version and
// endpoint type. The proxy presents the token on every call, and only when the
// pool requires agent auth; an empty token means the agent is not yet enrolled.
// The endpoint is validated again, so an agent stored before validation
// existed gets an error instead of a client.
//
// The connection of a gRPC agent is the agent's own, see Pool. A call in flight on
// a connection that Get closes, for an endpoint the agent left, fails.
func (p *Pool) Get(agentID, endpoint string, epType enum.EndpointType, apiVersion enum.APIVersion, token string) (AgentProxy, error) {
	p.mu.RLock()
	closed := p.closed
	p.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if !p.requireAuth {
		token = ""
	}
	switch apiVersion {
	case enum.APIVersionV1:
		return p.getV1(agentID, endpoint, epType, token)
	default:
		return nil, ErrUnsupportedAPIVersion
	}
}

func (p *Pool) getV1(agentID, endpoint string, epType enum.EndpointType, token string) (AgentProxy, error) {
	switch epType {
	case enum.EndpointHTTP:
		if err := validateEndpoint(endpoint, epType); err != nil {
			return nil, err
		}
		// An agent that moved from gRPC to HTTP has no use for its connection.
		p.Forget(agentID)
		// Wrap the shared client so every request carries the bearer header;
		// keep the bare shared client when there is no token (zero overhead).
		var client httpClient = p.httpCli
		if token != "" {
			client = &bearerClient{inner: p.httpCli, token: token}
		}
		return &httpProxyV1{
			endpoint: strings.TrimRight(endpoint, "/"),
			client:   client,
		}, nil
	case enum.EndpointGRPC:
		if err := validateEndpoint(endpoint, epType); err != nil {
			return nil, err
		}
		conn, err := p.grpcConn(agentID, endpoint)
		if err != nil {
			return nil, err
		}
		return &grpcProxyV1{conn: conn, token: token}, nil
	default:
		return nil, ErrUnsupportedEndpointType
	}
}

// validateEndpoint applies the discovery endpoint rules to a stored endpoint.
func validateEndpoint(endpoint string, epType enum.EndpointType) error {
	if err := model.ValidateAgentEndpoint(endpoint, epType); err != nil {
		return fmt.Errorf("%w: %w", ErrBadEndpointURL, err)
	}
	return nil
}

// grpcConn returns the agent's *grpc.ClientConn to the endpoint, or creates it. A
// connection of the agent to another endpoint is closed.
func (p *Pool) grpcConn(agentID, endpoint string) (*grpc.ClientConn, error) {
	// Normalise the agent-advertised endpoint: grpc-go's `NewClient`
	// wants a bare authority (`host:port`) and chokes on scheme-prefixed
	// URLs with "too many colons in address". Agents that accidentally
	// advertise `http://host:port` (legit for reqwest, wrong for gRPC)
	// still work after this normalisation.
	target := normalizeGrpcTarget(endpoint)

	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, ErrClosed
	}
	held, ok := p.grpcConns[agentID]
	p.mu.RUnlock()
	if ok && held.target == target {
		return held.conn, nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	held, ok = p.grpcConns[agentID]
	if ok && held.target == target {
		p.mu.Unlock()
		return held.conn, nil
	}
	// The explicit scheme keeps grpc-go's default DNS resolver for every
	// host: a bare "unix:1" would otherwise select the unix socket resolver.
	conn, err := grpc.NewClient(
		"dns:///"+target,
		grpc.WithTransportCredentials(p.grpcCreds),
		grpc.WithMaxHeaderListSize(maxAgentHeaderListBytes),
		// Align with solti-api's MAX_REQUEST_BYTES so a 5 MiB script body
		// (base64 → ~7 MiB on the wire) goes through cleanly. Without this
		// the agent's SubmitTaskResponse would still fit, but any response
		// approaching 4 MiB (ListTasks / ListTaskRuns on a busy agent)
		// would fail with `ResourceExhausted` on decode.
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxAgentMessageBytes),
			grpc.MaxCallSendMsgSize(maxAgentMessageBytes),
		),
	)
	if err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w %s: %v", ErrDial, endpoint, err)
	}
	p.grpcConns[agentID] = grpcEntry{target: target, conn: conn}
	p.mu.Unlock()
	if ok {
		// The agent advertises another endpoint: the connection to the old one is of no
		// use. Calls in flight on it fail.
		_ = held.conn.Close()
	}
	return conn, nil
}

// Forget closes the agent's pooled gRPC connection, if it has one: the agent was
// deleted, or its credential was reset. The next Get for the agent dials afresh.
// The pool holds no token (Get takes it on every call), so nothing it keeps
// presents a credential that was reset. A call in flight on the connection fails.
func (p *Pool) Forget(agentID string) {
	p.mu.RLock()
	_, ok := p.grpcConns[agentID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	p.mu.Lock()
	held, ok := p.grpcConns[agentID]
	delete(p.grpcConns, agentID)
	p.mu.Unlock()
	if ok {
		_ = held.conn.Close()
	}
}

// normalizeGrpcTarget strips HTTP-style scheme prefixes and trailing
// slashes from an agent-advertised endpoint, turning it into the bare
// `host:port` target that `grpc.NewClient` expects.
//
// Extra tolerance for mixed-transport agent configs: an agent may
// advertise `http://host:port` (copy-pasted from an HTTP example), and
// we still want the grpc client to be able to call back via gRPC.
func normalizeGrpcTarget(endpoint string) string {
	e := strings.TrimPrefix(endpoint, "https://")
	e = strings.TrimPrefix(e, "http://")
	e = strings.TrimRight(e, "/")
	return e
}

// Close releases all pooled connections.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	for _, held := range p.grpcConns {
		if err := held.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("%w %s: %v", ErrClose, held.target, err))
		}
	}
	p.grpcConns = nil
	p.httpCli.CloseIdleConnections()

	return errors.Join(errs...)
}
