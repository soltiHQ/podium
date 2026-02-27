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

	"github.com/soltiHQ/control-plane/domain/kind"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Pool manages shared outbound connections to agents.
//
// For HTTP it holds a single *http.Client whose Transport pools TCP connections.
// For gRPC it caches one *grpc.ClientConn per endpoint address.
type Pool struct {
	mu sync.RWMutex

	httpCli   *http.Client
	grpcConns map[string]*grpc.ClientConn
}

// NewPool creates a Pool with a configured HTTP transport.
func NewPool() *Pool {
	return &Pool{
		httpCli: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
			},
		},
		grpcConns: make(map[string]*grpc.ClientConn),
	}
}

// Get returns an AgentProxy for the given endpoint, selecting the implementation
// based on api version and endpoint type.
func (p *Pool) Get(endpoint string, epType kind.EndpointType, apiVersion kind.APIVersion) (AgentProxy, error) {
	switch apiVersion {
	case kind.APIVersionV1:
		return p.getV1(endpoint, epType)
	default:
		return nil, ErrUnsupportedAPIVersion
	}
}

func (p *Pool) getV1(endpoint string, epType kind.EndpointType) (AgentProxy, error) {
	switch epType {
	case kind.EndpointHTTP:
		return &httpProxyV1{
			endpoint: strings.TrimRight(endpoint, "/"),
			client:   p.httpCli,
		}, nil
	case kind.EndpointGRPC:
		conn, err := p.grpcConn(endpoint)
		if err != nil {
			return nil, err
		}
		return &grpcProxyV1{conn: conn}, nil
	default:
		return nil, ErrUnsupportedEndpointType
	}
}

// grpcConn returns a cached *grpc.ClientConn or creates one.
func (p *Pool) grpcConn(endpoint string) (*grpc.ClientConn, error) {
	p.mu.RLock()
	conn, ok := p.grpcConns[endpoint]
	p.mu.RUnlock()
	if ok {
		return conn, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if conn, ok = p.grpcConns[endpoint]; ok {
		return conn, nil
	}
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %v", ErrDial, endpoint, err)
	}
	p.grpcConns[endpoint] = conn
	return conn, nil
}

// Close releases all pooled connections.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	for ep, conn := range p.grpcConns {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("%w %s: %v", ErrClose, ep, err))
		}
	}
	p.grpcConns = nil
	p.httpCli.CloseIdleConnections()

	return errors.Join(errs...)
}
