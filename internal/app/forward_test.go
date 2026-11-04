package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/tlsconf"
)

// testCA is a certificate authority made when a test runs. Its key stays in
// memory; its certificate is written to a file of the test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // its certificate, PEM
}

// newTestCA makes a CA valid for the next hour.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "podium app test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := &testCA{cert: cert, key: key, file: filepath.Join(t.TempDir(), "ca.crt")}
	writePEM(t, ca.file, "CERTIFICATE", der)
	return ca
}

// issue writes a server and client certificate of ca for hosts, each an IP or
// a DNS subject alternative name, and its key, and returns their files.
func (ca *testCA) issue(t *testing.T, hosts ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "PRIVATE KEY", keyDER)
	return certFile, keyFile
}

// writePEM writes der to path as one PEM block of typ, readable by its owner
// only.
func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// follower is the leadership of a raft follower whose leader is at the raft
// address leader.
type follower struct{ leader string }

func (follower) AmLeader() bool          { return false }
func (f follower) CurrentLeader() string { return f.leader }
func (follower) WhenLeader(ctx context.Context, _ func(context.Context) error) error {
	<-ctx.Done()
	return nil
}

// followerOf is the leadership of a follower of the node whose leadership it
// holds: the follower leads nothing, and its leader is that node's leader.
type followerOf struct{ cluster.Leadership }

func (followerOf) AmLeader() bool { return false }

// A follower knows its leader by the raft address the leader advertises, as
// written, so with tls.server set it checks the leader's certificate for that
// host: a write reaches a leader that advertises a host name with a
// certificate for that name, and none reaches a leader whose certificate
// names only the IP the name resolves to.
func TestLeaderForwardingToALeaderNamedByHost(t *testing.T) {
	ca := newTestCA(t)
	raftCert, raftKey := ca.issue(t, "localhost")
	bind := loopbackAddrs(t, 1)[0]
	_, raftPort, err := net.SplitHostPort(bind)
	if err != nil {
		t.Fatal(err)
	}
	advertise := net.JoinHostPort("localhost", raftPort)
	raftCfg := cluster.DefaultConfig()
	raftCfg.Backend = cluster.BackendRaft
	raftCfg.Raft = cluster.RaftConfig{
		NodeID: "n1", BindAddr: bind, AdvertiseAddr: advertise, DataDir: filepath.Join(t.TempDir(), "raft"),
		ElectionTimeoutMs: 50, HeartbeatTimeoutMs: 50,
		TLS: cluster.RaftTLSConfig{CertFile: raftCert, KeyFile: raftKey, CAFile: ca.file},
	}
	raftCfg.Discovery.Peers = []string{"n1=" + advertise}
	_, leadership, hub, shutdown, err := buildCluster(raftCfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		hub.Close()
		shutdown()
	})
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.Now().Add(5 * time.Second); !leadership.AmLeader(); <-tick.C {
		if time.Now().After(deadline) {
			t.Fatal("the raft node did not lead within 5s")
		}
	}

	followerCert, followerKey := ca.issue(t, "127.0.0.1")
	for _, tc := range []struct {
		name      string
		hosts     []string // the subject alternative names of the leader's certificate
		forwarded bool
	}{
		{"a leader certificate for the host name", []string{"localhost"}, true},
		{"a leader certificate for its IP only", []string{"127.0.0.1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaderCert, leaderKey := ca.issue(t, tc.hosts...)
			cert, err := tls.LoadX509KeyPair(leaderCert, leaderKey)
			if err != nil {
				t.Fatal(err)
			}
			reached := make(chan struct{}, 1)
			leader := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached <- struct{}{}
				w.WriteHeader(http.StatusAccepted)
			}))
			leader.Config.ErrorLog = log.New(io.Discard, "", 0)
			leader.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
			leader.StartTLS()
			t.Cleanup(leader.Close)
			_, port, err := net.SplitHostPort(leader.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}

			cfg := config.Default()
			cfg.HTTP.Addr = ":" + port
			cfg.TLS.Server = tlsconf.ServerConfig{CertFile: followerCert, KeyFile: followerKey}
			cfg.TLS.Client = tlsconf.ClientConfig{CAFile: ca.file}
			clientTLS, err := cfg.TLS.Client.Build()
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			mainHandlerOf(t, cfg, inmemory.New(), zerolog.Nop(), followerOf{leadership}, clientTLS).
				ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader("{}")))
			switch want := map[bool]int{true: http.StatusAccepted, false: http.StatusBadGateway}[tc.forwarded]; {
			case w.Code != want:
				t.Fatalf("follower of the leader at %s answered %d %q, want %d", leadership.CurrentLeader(), w.Code, w.Body.String(), want)
			case !tc.forwarded && len(reached) > 0:
				t.Fatal("the leader got the write")
			}
		})
	}
}

// With tls.server set, a follower forwards a write to the leader over TLS with
// the tls.client configuration: it verifies the leader's certificate against
// tls.client.ca_file, for the leader's host, and presents tls.client's
// certificate when the leader asks for one. So a write reaches a leader whose
// certificate the cluster CA signed, while a leader with a certificate of
// another CA, or one that wants a client certificate the follower lacks, gets
// nothing and the follower answers 502. The main and the discovery listener
// forward alike.
func TestLeaderForwardingTrustsClusterCA(t *testing.T) {
	clusterCA, otherCA := newTestCA(t), newTestCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(clusterCA.cert)
	followerCert, followerKey := clusterCA.issue(t, "127.0.0.1")
	for _, listener := range []struct {
		name, path string
		addr       func(*config.Config) *string // the listen address whose port the follower forwards to
		build      func(t *testing.T, cfg config.Config, forwardTLS *tls.Config) http.Handler
	}{
		{"main", "/api/v1/users", func(c *config.Config) *string { return &c.HTTP.Addr }, func(t *testing.T, cfg config.Config, forwardTLS *tls.Config) http.Handler {
			return mainHandlerOf(t, cfg, inmemory.New(), zerolog.Nop(), follower{leader: "127.0.0.1:7000"}, forwardTLS)
		}},
		{"discovery", "/api/v1/discovery/sync", func(c *config.Config) *string { return &c.HTTPDiscovery.Addr }, func(t *testing.T, cfg config.Config, forwardTLS *tls.Config) http.Handler {
			hub := event.NewHub(zerolog.Nop())
			t.Cleanup(hub.Close)
			h, srv := buildDiscoveryListeners(zerolog.Nop(), agent.New(inmemory.New(), zerolog.Nop()), hub, follower{leader: "127.0.0.1:7000"}, cfg, nil, nil, forwardTLS, nil)
			t.Cleanup(srv.Stop)
			return h
		}},
	} {
		for _, tc := range []struct {
			name        string
			leaderCA    *testCA // signs the leader's certificate
			wantsClient bool    // the leader wants a client certificate of the cluster CA
			clientCert  bool    // tls.client has one
			forwarded   bool
		}{
			{"leader certificate of the cluster CA", clusterCA, false, true, true},
			{"leader certificate of another CA", otherCA, false, true, false},
			{"leader that wants a client certificate", clusterCA, true, true, true},
			{"leader that wants a client certificate the follower lacks", clusterCA, true, false, false},
		} {
			t.Run(listener.name+"/"+tc.name, func(t *testing.T) {
				leaderCert, leaderKey := tc.leaderCA.issue(t, "127.0.0.1")
				cert, err := tls.LoadX509KeyPair(leaderCert, leaderKey)
				if err != nil {
					t.Fatal(err)
				}
				reached := make(chan string, 1)
				leader := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached <- r.URL.Path
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, "the leader")
				}))
				// The refused handshakes are what the cases expect, not news.
				leader.Config.ErrorLog = log.New(io.Discard, "", 0)
				leader.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
				if tc.wantsClient {
					leader.TLS.ClientCAs, leader.TLS.ClientAuth = pool, tls.RequireAndVerifyClientCert
				}
				leader.StartTLS()
				t.Cleanup(leader.Close)
				_, port, err := net.SplitHostPort(leader.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}

				cfg := config.Default()
				*listener.addr(&cfg) = ":" + port
				cfg.TLS.Server = tlsconf.ServerConfig{CertFile: followerCert, KeyFile: followerKey}
				cfg.TLS.Client = tlsconf.ClientConfig{CAFile: clusterCA.file}
				if tc.clientCert {
					cfg.TLS.Client.CertFile, cfg.TLS.Client.KeyFile = followerCert, followerKey
				}
				clientTLS, err := cfg.TLS.Client.Build()
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				listener.build(t, cfg, clientTLS).ServeHTTP(w, httptest.NewRequest(http.MethodPost, listener.path, strings.NewReader("{}")))

				if !tc.forwarded {
					if w.Code != http.StatusBadGateway {
						t.Fatalf("follower answered %d %q, want %d", w.Code, w.Body.String(), http.StatusBadGateway)
					}
					select {
					case path := <-reached:
						t.Fatalf("the leader got %s", path)
					default:
					}
					return
				}
				if w.Code != http.StatusAccepted || w.Body.String() != "the leader" {
					t.Fatalf("follower answered %d %q, want the leader's %d", w.Code, w.Body.String(), http.StatusAccepted)
				}
				if path := <-reached; path != listener.path {
					t.Fatalf("the leader got %s, want %s", path, listener.path)
				}
			})
		}
	}
}
