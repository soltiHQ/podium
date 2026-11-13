package app

import (
	"crypto/tls"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/tlsconf"
)

// sharedFiles are the TLS files of a configuration with agent mTLS on: the
// node's server certificate and key, of the CA whose bundle is ca, and the
// bundles of the same CA at another path and of another CA.
type sharedFiles struct {
	cert, key   string
	ca, caAgain string
	otherCA     string
}

// newSharedFiles writes the files of sharedFiles for a test.
func newSharedFiles(t *testing.T) sharedFiles {
	t.Helper()
	ca := newTestCA(t)
	cert, key := ca.issue(t, "127.0.0.1")
	bundle, err := os.ReadFile(ca.file)
	if err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(t.TempDir(), "elsewhere", "agents-ca.pem")
	if err := os.MkdirAll(filepath.Dir(again), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(again, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return sharedFiles{cert: cert, key: key, ca: ca.file, caAgain: again, otherCA: newTestCA(t).file}
}

// sharedBase is a configuration that sets every setting replicas must share,
// with agent mTLS on over the CA of files.
func sharedBase(files sharedFiles) config.Config {
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("fingerprint-test-signing-secret", 2)
	cfg.Auth.CookieSecure = auth.CookieSecureAuto
	cfg.Auth.Issuer, cfg.Auth.Audience = "podium-test", "podium-test-api"
	cfg.AgentAuth.Require = true
	cfg.AgentAuth.EnrollSecret = strings.Repeat("fingerprint-test-enroll-secret", 2)
	cfg.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/16"}
	cfg.CORS = middleware.CORSConfig{
		AllowOrigins:     []string{"https://console.example", "https://admin.example"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           time.Hour,
	}
	cfg.TLS.Server = tlsconf.ServerConfig{CertFile: files.cert, KeyFile: files.key, ClientCAFile: files.ca}
	return cfg
}

// digestsOf returns the digests of the settings of cfg that replicas must share.
func digestsOf(t *testing.T, cfg config.Config) cluster.Digests {
	t.Helper()
	cas, err := agentCAs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := sharedSettings(cfg, cas)
	if err != nil {
		t.Fatal(err)
	}
	return shared.Digests()
}

// Each setting that replicas must share counts in the fingerprint, and in the
// digest of its own group only: a change of any one changes the fingerprint and
// its group's digest, and leaves the other groups' digests as they were. Agent
// mTLS counts by whether it is on and by the certificates of its CA bundle:
// another CA counts, and so do methods or origins in another case, which apply
// otherwise. No other setting counts, the node's own server certificate and
// tls.client included. Settings that apply alike digest alike: trusted proxies
// and CORS origins in another order or listed twice, proxies written in another
// form of the same prefix, header names in another case, header lists split
// across entries otherwise, an empty CORS list and the defaults CORS applies
// for it, a max_age of zero and the 12 hours CORS applies for it, max_ages in
// one whole second, which is what CORS sends, an empty issuer or audience and
// its default, and the same CA bundle at another path, or with a duplicate and
// blocks that the pool skips.
func TestFingerprintChangesWithEachSetting(t *testing.T) {
	files := newSharedFiles(t)
	base := digestsOf(t, sharedBase(files))
	if base.Fingerprint() == (cluster.Digests{}).Fingerprint() {
		t.Fatal("the base configuration digests as nothing")
	}
	for _, tc := range []struct {
		setting string
		group   string // "" for a setting that does not count
		edit    func(*config.Config)
	}{
		{"auth.jwt_secret", cluster.GroupAuth, func(c *config.Config) { c.Auth.JWTSecret = strings.Repeat("another-test-signing-secret", 2) }},
		{"auth.cookie_secure", cluster.GroupAuth, func(c *config.Config) { c.Auth.CookieSecure = auth.CookieSecureAlways }},
		{"auth.issuer", cluster.GroupAuth, func(c *config.Config) { c.Auth.Issuer = "another-issuer" }},
		{"auth.audience", cluster.GroupAuth, func(c *config.Config) { c.Auth.Audience = "another-audience" }},
		{"agent_auth.require", cluster.GroupAgentAuth, func(c *config.Config) { c.AgentAuth.Require = false }},
		{"agent_auth.enroll_secret", cluster.GroupAgentAuth, func(c *config.Config) { c.AgentAuth.EnrollSecret = strings.Repeat("another-test-enroll-secret", 2) }},
		{"trusted_proxies: another entry", cluster.GroupProxies, func(c *config.Config) { c.TrustedProxies = append(c.TrustedProxies, "10.0.0.2") }},
		{"trusted_proxies: a prefix of another length", cluster.GroupProxies, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/24"} }},
		{"cors.allow_origins", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowOrigins = []string{"https://console.example"} }},
		{"cors.allow_origins: an origin in another case", cluster.GroupCORS, func(c *config.Config) {
			c.CORS.AllowOrigins = []string{"https://Console.example", "https://admin.example"}
		}},
		{"cors.allow_methods", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowMethods = append(c.CORS.AllowMethods, "DELETE") }},
		{"cors.allow_methods: a method in another case", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowMethods = []string{"get", "POST"} }},
		{"cors.allow_headers", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowHeaders = append(c.CORS.AllowHeaders, "If-Match") }},
		{"cors.expose_headers", cluster.GroupCORS, func(c *config.Config) { c.CORS.ExposeHeaders = append(c.CORS.ExposeHeaders, "ETag") }},
		{"cors.allow_credentials", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowCredentials = false }},
		{"cors.max_age", cluster.GroupCORS, func(c *config.Config) { c.CORS.MaxAge = 2 * time.Hour }},
		{"tls.server.client_ca_file: agent mTLS off", cluster.GroupAgentTLS, func(c *config.Config) { c.TLS.Server.ClientCAFile = "" }},
		{"tls.server.client_ca_file: another CA", cluster.GroupAgentTLS, func(c *config.Config) { c.TLS.Server.ClientCAFile = files.otherCA }},
		{"tls.server.cert_file and key_file", "", func(c *config.Config) {
			c.TLS.Server.CertFile, c.TLS.Server.KeyFile = newTestCA(t).issue(t, "127.0.0.1")
		}},
		{"tls.client", "", func(c *config.Config) { c.TLS.Client = tlsconf.ClientConfig{CAFile: files.otherCA} }},
		{"auth.access_ttl", "", func(c *config.Config) { c.Auth.AccessTTL = time.Minute }},
		{"lifecycle.delete_after", "", func(c *config.Config) { c.Lifecycle.DeleteAfter = time.Hour }},
		{"cluster.raft.node_id", "", func(c *config.Config) { c.Cluster.Raft.NodeID = "n9" }},
		{"http.addr", "", func(c *config.Config) { c.HTTP.Addr = ":9090" }},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			cfg := sharedBase(files)
			tc.edit(&cfg)
			got := digestsOf(t, cfg)
			if tc.group == "" {
				if got != base {
					t.Fatalf("%s, which replicas need not share, changes the digests of %v", tc.setting, base.Differ(got))
				}
				return
			}
			if got.Fingerprint() == base.Fingerprint() {
				t.Fatalf("%s leaves the fingerprint as it was", tc.setting)
			}
			if differ := base.Differ(got); len(differ) != 1 || differ[0] != tc.group {
				t.Fatalf("%s changes the digests of %v, want %s only", tc.setting, differ, tc.group)
			}
		})
	}

	// mixedBundle is the bundle of files' CA twice, with a key block and an
	// encrypted block between, which the listeners' pool skips.
	mixedBundle := func() string {
		bundle, err := os.ReadFile(files.ca)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(bundle)
		mixed := append([]byte{}, bundle...)
		mixed = append(mixed, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")})...)
		mixed = append(mixed, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: block.Bytes})...)
		mixed = append(mixed, bundle...)
		path := filepath.Join(t.TempDir(), "mixed.pem")
		if err := os.WriteFile(path, mixed, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}()
	for _, tc := range []struct {
		name string
		a, b func(*config.Config)
	}{
		{"trusted_proxies in another order", nil, func(c *config.Config) { c.TrustedProxies = []string{"192.168.0.0/16", "10.0.0.1"} }},
		{"trusted_proxies with an entry twice", nil, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/16", "10.0.0.1"} }},
		{"trusted_proxies written otherwise", nil, func(c *config.Config) { c.TrustedProxies = []string{"::ffff:10.0.0.1", " 192.168.7.9/16 "} }},
		{"an IP and its /32", nil, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1/32", "192.168.0.0/16"} }},
		{"cors.allow_origins in another order", nil, func(c *config.Config) {
			c.CORS.AllowOrigins = []string{"https://admin.example", "https://console.example"}
		}},
		{"cors.allow_origins with an origin twice", nil, func(c *config.Config) {
			c.CORS.AllowOrigins = []string{"https://console.example", "https://admin.example", "https://console.example"}
		}},
		{"cors.allow_methods in one entry, in another order", nil, func(c *config.Config) { c.CORS.AllowMethods = []string{"POST, GET"} }},
		{"cors.allow_headers in another case and order", nil, func(c *config.Config) { c.CORS.AllowHeaders = []string{"content-type", "AUTHORIZATION"} }},
		{"cors.expose_headers in another case", nil, func(c *config.Config) { c.CORS.ExposeHeaders = []string{"x-request-id"} }},
		{"cors lists empty and as the defaults", func(c *config.Config) {
			c.CORS.AllowMethods, c.CORS.AllowHeaders, c.CORS.ExposeHeaders = nil, nil, nil
		}, func(c *config.Config) {
			c.CORS.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
			c.CORS.AllowHeaders = []string{"Authorization", "Content-Type", "X-Request-Id", "If-Match"}
			c.CORS.ExposeHeaders = []string{"X-Request-Id", "ETag"}
		}},
		{"cors.max_age zero and 12 hours", func(c *config.Config) { c.CORS.MaxAge = 0 }, func(c *config.Config) { c.CORS.MaxAge = 12 * time.Hour }},
		{"cors.max_age within one second", func(c *config.Config) { c.CORS.MaxAge = 90 * time.Second }, func(c *config.Config) { c.CORS.MaxAge = 90*time.Second + 900*time.Millisecond }},
		{"auth.issuer and auth.audience empty and their defaults", func(c *config.Config) {
			c.Auth.Issuer, c.Auth.Audience = "", ""
		}, func(c *config.Config) {
			defaults := auth.Config{}.WithDefaults()
			c.Auth.Issuer, c.Auth.Audience = defaults.Issuer, defaults.Audience
		}},
		{"the same client CA at another path", nil, func(c *config.Config) { c.TLS.Server.ClientCAFile = files.caAgain }},
		{"the same client CA twice, with blocks the pool skips", nil, func(c *config.Config) { c.TLS.Server.ClientCAFile = mixedBundle }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := sharedBase(files), sharedBase(files)
			for _, edit := range []struct {
				cfg  *config.Config
				edit func(*config.Config)
			}{{&a, tc.a}, {&b, tc.b}} {
				if edit.edit != nil {
					edit.edit(edit.cfg)
				}
			}
			if da, db := digestsOf(t, a), digestsOf(t, b); da != db {
				t.Fatalf("%s digest otherwise, in %v", tc.name, da.Differ(db))
			}
		})
	}
}

// A raft node reads the agents' CA bundle as the discovery listeners build
// their pool from it, so a bundle that cannot be read, or holds no
// certificate, fails the start as it does in memory: with the error of
// tlsconf, which names the bundle. In raft mode New fails before the raft node
// starts.
func TestUnreadableAgentCABundleFailsTheStart(t *testing.T) {
	files := newSharedFiles(t)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bundle, want string
	}{
		{"missing", filepath.Join(t.TempDir(), "missing.pem"), "no such file"},
		{"without a certificate", empty, "no certificates found in " + empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sharedBase(files)
			cfg.TLS.Server.ClientCAFile = tc.bundle
			_, err := agentCAs(cfg)
			if err == nil || !strings.Contains(err.Error(), "tls server: client CA: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("agentCAs() = %v, want the error of tlsconf saying %q", err, tc.want)
			}
			if _, buildErr := cfg.TLS.Server.Build(); buildErr == nil || buildErr.Error() != err.Error() {
				t.Fatalf("Build() = %v, want the same error as the digest, %v", buildErr, err)
			}

			raft := raftConfigs(t, "n1")[0]
			node := config.Default()
			node.Cluster = raft
			node.Auth.JWTSecret = cfg.Auth.JWTSecret
			node.TLS.Server = cfg.TLS.Server
			node.HTTP.Addr, node.HTTPDiscovery.Addr, node.GRPC.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
			if _, err := New(t.Context(), node, zerolog.Nop()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() of a raft node = %v, want an error saying %q", err, tc.want)
			}
			if _, err := os.Stat(raft.Raft.DataDir); !os.IsNotExist(err) {
				t.Fatalf("the raft data directory of the refused node: %v, want none", err)
			}
		})
	}
}

// A raft node reads the agents' CA bundle once, before it starts, for its
// digests, and builds the pool of its agent-facing listeners from the
// certificates it read: however the file changes after that read, the
// listeners verify agents against the certificates the node's digest names.
// Memory builds the pool from the file, as it always did.
func TestRaftNodeReadsTheAgentCABundleOnce(t *testing.T) {
	files := newSharedFiles(t)
	cfg := sharedBase(files)
	bundle := filepath.Join(t.TempDir(), "agents-ca.pem")
	pemBytes, err := os.ReadFile(files.ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TLS.Server.ClientCAFile = bundle
	cas, err := agentCAs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	read, err := cfg.TLS.Server.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}

	raft, err := agentListenerTLS(cfg, true, cas)
	if err != nil {
		t.Fatalf("the raft node's listener TLS once the bundle it read is gone: %v", err)
	}
	if raft.ClientAuth != tls.RequireAndVerifyClientCert || !raft.ClientCAs.Equal(read.ClientCAs) {
		t.Fatal("the raft node's listeners do not verify agents against the certificates it read")
	}
	if _, err := agentListenerTLS(cfg, false, nil); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("memory's listener TLS once the bundle is gone = %v, want the error of the file it reads", err)
	}
}
