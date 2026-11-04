package raft

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// The pool size and the I/O timeout of the raft transport, as New has always
// given them to hashicorp/raft.
const (
	transportMaxPool = 3
	transportTimeout = 10 * time.Second
)

// errNotAdvertisable refuses an advertise address peers cannot dial, as
// hashicorp/raft's TCP transport does.
var errNotAdvertisable = errors.New("local bind address is not advertisable")

// transportTLS is mutual TLS between nodes, over TLS 1.3: both sides present
// the node's one certificate and verify the peer's against the cluster CA.
type transportTLS struct {
	server *tls.Config // requires a client certificate the CA verifies
	client *tls.Config // verifies the server's certificate for the dialed host
}

// transportSecurity returns the mutual TLS of cfg, or nil for plain TCP, which
// cfg must allow. A node that names any TLS file never runs plain TCP.
func transportSecurity(cfg Config) (*transportTLS, error) {
	switch {
	case cfg.TLS.Enabled():
		return loadTLS(cfg.TLS)
	case cfg.AllowInsecureTransport:
		return nil, nil
	}
	return nil, fmt.Errorf("raft: transport: %w", cluster.ErrRaftTLSRequired)
}

// loadTLS reads the node's certificate and key and the cluster CA of files.
// Its errors name the files, never what they hold.
func loadTLS(files cluster.RaftTLSConfig) (*transportTLS, error) {
	if files.CertFile == "" || files.KeyFile == "" || files.CAFile == "" {
		return nil, fmt.Errorf("raft: tls: %w", cluster.ErrRaftTLSRequired)
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("raft: tls: load cert_file and key_file: %w", err)
	}
	pem, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("raft: tls: read ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("raft: tls: no certificate in ca_file %s", files.CAFile)
	}
	return &transportTLS{
		server: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		},
		client: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
		},
	}, nil
}

// tlsStreamLayer is the hraft.StreamLayer of the raft transport over mutual
// TLS. It accepts on a tls.Listen listener: the handshake of an accepted
// connection runs on its first read, in the goroutine the transport gives it,
// so a slow peer holds up no other. It dials with tls.DialWithDialer, whose
// timeout covers the handshake.
type tlsStreamLayer struct {
	net.Listener
	advertise *net.TCPAddr
	client    *tls.Config
}

// Dial implements hraft.StreamLayer. The client side sets no ServerName, so
// tls.DialWithDialer verifies the peer's certificate for the host of address:
// the peer's address in the raft configuration, as discovery listed it.
func (l *tlsStreamLayer) Dial(address hraft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", string(address), l.client)
}

// Addr implements net.Listener with the address peers dial: raft takes it for
// this node's own.
func (l *tlsStreamLayer) Addr() net.Addr { return l.advertise }

// newTransport opens the raft transport on bind, which peers reach at
// advertise: over mutual TLS, or plain TCP when security is nil.
func newTransport(bind string, advertise *net.TCPAddr, security *transportTLS) (*hraft.NetworkTransport, error) {
	if security == nil {
		return hraft.NewTCPTransport(bind, advertise, transportMaxPool, transportTimeout, os.Stderr)
	}
	if advertise.IP == nil || advertise.IP.IsUnspecified() {
		return nil, errNotAdvertisable
	}
	listener, err := tls.Listen("tcp", bind, security.server)
	if err != nil {
		return nil, err
	}
	// A nil Logger logs to stderr, as NewTCPTransport is told to.
	return hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{
		Stream:  &tlsStreamLayer{Listener: listener, advertise: advertise, client: security.client},
		MaxPool: transportMaxPool,
		Timeout: transportTimeout,
	}), nil
}
