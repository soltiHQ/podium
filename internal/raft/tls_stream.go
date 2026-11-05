package raft

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

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

// handshakeTimeout bounds the TLS handshake of a connection the node accepts.
// It is a variable so that tests can shorten it; New reads it when it opens the
// transport.
var handshakeTimeout = transportTimeout

// probeTimeout bounds a probe of a peer's address (see probe): the dial, the TLS
// handshake and the close.
const probeTimeout = 5 * time.Second

// ErrNodeCertificate indicates a node certificate its peers would refuse: not
// signed by the CA of ca_file, outside its validity period, not for both server
// and client authentication, or not for the host of the node's advertise
// address as written.
var ErrNodeCertificate = errors.New("the raft node certificate would be refused by its peers")

// transportTLS is mutual TLS between nodes, over TLS 1.3: both sides present
// the node's one certificate and verify the peer's against the cluster CA.
type transportTLS struct {
	server *tls.Config // requires a client certificate the CA verifies
	client *tls.Config // verifies the server's certificate for the dialed host
}

// transportSecurity returns the mutual TLS of cfg, whose certificate must be
// one the node's peers accept at advertise, or nil for plain TCP, which cfg
// must allow. A node that names any TLS file never runs plain TCP.
func transportSecurity(cfg Config, advertise string) (*transportTLS, error) {
	switch {
	case cfg.TLS.Enabled():
		return loadTLS(cfg.TLS, advertise)
	case cfg.AllowInsecureTransport:
		return nil, nil
	}
	return nil, fmt.Errorf("raft: transport: %w", cluster.ErrRaftTLSRequired)
}

// loadTLS reads the node's certificate and key and the cluster CA of files, and
// checks the certificate as peers will check it at advertise (see
// checkCertificate). Its errors name the files, never what they hold.
func loadTLS(files cluster.RaftTLSConfig, advertise string) (*transportTLS, error) {
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
	if err := checkCertificate(cert, pool, advertise); err != nil {
		return nil, err
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

// checkCertificate checks cert as the node's peers will: against the CA pool,
// now, for client authentication when the node dials them and for server
// authentication when they dial it, at the host of advertise, written as the
// peers list it. A node whose peers refuse its certificate would otherwise
// start and stay cut off. Go's checks cover every certificate of the chain.
func checkCertificate(cert tls.Certificate, pool *x509.CertPool, advertise string) error {
	host, _, err := net.SplitHostPort(advertise)
	if err != nil {
		return fmt.Errorf("raft: advertise address %q: %w", advertise, err)
	}
	chain := make([]*x509.Certificate, 0, len(cert.Certificate))
	for _, der := range cert.Certificate {
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("raft: tls: parse cert_file: %w", err)
		}
		chain = append(chain, parsed)
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	for _, check := range []struct {
		usage x509.ExtKeyUsage
		role  string
	}{
		{x509.ExtKeyUsageServerAuth, "server"},
		{x509.ExtKeyUsageClientAuth, "client"},
	} {
		// One usage at a time: Verify accepts a chain that allows any one of
		// the usages it is given.
		opts := x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{check.usage}}
		if _, err := chain[0].Verify(opts); err != nil {
			return fmt.Errorf("raft: tls: cert_file as a %s certificate: %w: %w", check.role, ErrNodeCertificate, err)
		}
	}
	if err := chain[0].VerifyHostname(host); err != nil {
		return fmt.Errorf("raft: tls: cert_file for the advertise host %q: %w: %w", host, ErrNodeCertificate, err)
	}
	return nil
}

// advertiseAddr is the address peers dial, as written: the address of the
// node in the raft configuration. raft takes the transport's address for the
// node's own, which a leader announces, so its followers know it by the name
// the configuration gives it, as a follower's HTTP forward needs.
type advertiseAddr string

func (a advertiseAddr) Network() string { return "tcp" }
func (a advertiseAddr) String() string  { return string(a) }

// streamLayer is the hraft.StreamLayer of the raft transport, over mutual TLS
// or, with no security, plain TCP as hashicorp/raft's TCP transport.
type streamLayer struct {
	net.Listener // TCP
	advertise    advertiseAddr
	security     *transportTLS // nil for plain TCP
	handshake    time.Duration // the bound of an accepted connection's handshake
	answer       []byte        // the answer to a configuration request, nil for none (see answerConn)
}

// Accept implements net.Listener. A TLS connection's handshake runs on its
// first read or write, in the goroutine the transport gives the connection,
// and must finish within the handshake bound (see serverConn). It never runs
// here: hashicorp/raft accepts in one loop, which a stalled peer would hold up.
// Every connection, over TLS or plain TCP, answers a configuration request
// before hashicorp/raft reads it, and passes every RPC through (answerConn).
func (l *streamLayer) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	if l.security != nil {
		conn = &serverConn{Conn: tls.Server(conn, l.security.server), timeout: l.handshake}
	}
	return &answerConn{Conn: conn, answer: l.answer}, nil
}

// Dial implements hraft.StreamLayer. Over TLS the client side sets no
// ServerName, so tls.DialWithDialer verifies the peer's certificate for the
// host of address, the peer's address in the raft configuration, as discovery
// listed it, and the dial timeout covers the handshake.
func (l *streamLayer) Dial(address hraft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if l.security == nil {
		return dialer.Dial("tcp", string(address))
	}
	return tls.DialWithDialer(dialer, "tcp", string(address), l.security.client)
}

// Addr implements net.Listener with the advertise address as written.
func (l *streamLayer) Addr() net.Addr { return l.advertise }

// dialPeer dials address as Dial does, within ctx: over TLS, the handshake
// verifies the peer's certificate against the cluster CA, for the host of
// address as written, as a dial of raft would, and presents this node's
// certificate. security nil dials plain TCP.
func dialPeer(ctx context.Context, address string, security *transportTLS) (net.Conn, error) {
	dialer := &net.Dialer{}
	if security == nil {
		return dialer.DialContext(ctx, "tcp", address)
	}
	return (&tls.Dialer{NetDialer: dialer, Config: security.client}).DialContext(ctx, "tcp", address)
}

// probe dials address as Dial does, within ctx (dialPeer). It then closes its
// writing side, with a close_notify over TLS, and reads until the peer closes
// too: over TLS 1.3 a client's handshake ends before the server has checked the
// client's certificate, so a peer that refuses this node's certificate fails the
// probe with its alert only then. The transport of a node it probes reads the
// end of a stream, and logs no error for it. security nil probes plain TCP.
func probe(ctx context.Context, address string, security *transportTLS) error {
	conn, err := dialPeer(ctx, address, security)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("%T cannot close its writing side", conn)
	}
	if err := half.CloseWrite(); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, conn)
	return err
}

// serverConn is a TLS connection the node accepted. Its handshake, which
// tls.Conn would run unbounded on the first read or write, must finish within
// timeout, or the connection is closed: a host that connects and stalls, or
// stops halfway through its ClientHello, holds the transport's goroutine, file
// and buffer no longer. The bound ends with the handshake, so the connection
// may then sit idle between RPCs.
type serverConn struct {
	*tls.Conn
	timeout time.Duration
	once    sync.Once
	err     error
}

// handshake runs the handshake once. HandshakeContext closes the connection
// when its context ends first.
func (c *serverConn) handshake() error {
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		c.err = c.HandshakeContext(ctx)
	})
	return c.err
}

func (c *serverConn) Read(b []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *serverConn) Write(b []byte) (int, error) {
	if err := c.handshake(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// newTransport opens the raft transport on bind, over mutual TLS, or plain TCP
// when security is nil. Peers reach it at advertise, as written, which resolves
// to resolved: an address they can dial. It answers a configuration request
// with answer, or answers none when answer is nil (see answerConn). The
// transport logs into logger: every connection it refuses or that times out in
// its handshake is one ERROR line.
func newTransport(bind, advertise string, resolved *net.TCPAddr, security *transportTLS, answer []byte, logger zerolog.Logger) (*hraft.NetworkTransport, error) {
	if resolved.IP == nil || resolved.IP.IsUnspecified() {
		return nil, errNotAdvertisable
	}
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	return hraft.NewNetworkTransportWithConfig(&hraft.NetworkTransportConfig{
		Stream: &streamLayer{
			Listener:  listener,
			advertise: advertiseAddr(advertise),
			security:  security,
			handshake: handshakeTimeout,
			answer:    answer,
		},
		MaxPool: transportMaxPool,
		Timeout: transportTimeout,
		Logger:  newHCLog(logger, "raft-net"),
	}), nil
}
