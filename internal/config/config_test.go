package config

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
)

func TestLoadTrustedProxies(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("trusted_proxies:\n  - 10.0.0.1\n  - 2001:db8::/32\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, env string
		want            []string
		errorPath       string
	}{
		{name: "default", want: nil},
		{name: "yaml", path: yamlPath, want: []string{"10.0.0.1", "2001:db8::/32"}},
		{name: "env is comma-separated", env: "10.0.0.1,192.168.0.0/16", want: []string{"10.0.0.1", "192.168.0.0/16"}},
		{name: "env entries are trimmed", env: "10.0.0.1, 10.0.0.2", want: []string{"10.0.0.1", "10.0.0.2"}},
		{name: "invalid entry", env: "10.0.0.1,proxy.example.com", errorPath: "config.trusted_proxies"},
		{name: "empty entry", env: "10.0.0.1, ", errorPath: "config.trusted_proxies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", tc.path)
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			if tc.env != "" {
				t.Setenv("SOLTI_TRUSTED_PROXIES", tc.env)
			}
			cfg, err := load("")
			if tc.errorPath != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorPath) {
					t.Fatalf("want a %s error, got %v", tc.errorPath, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.TrustedProxies, tc.want) {
				t.Fatalf("trusted_proxies = %q, want %q", cfg.TrustedProxies, tc.want)
			}
		})
	}
}

// load is [Load] with its warnings dropped.
func load(path string) (Config, error) { return Load(path, zerolog.Nop()) }

func TestLoadReadsTheConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("trusted_proxies:\n  - 10.0.0.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	cfg, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.9"}; !slices.Equal(cfg.TrustedProxies, want) {
		t.Fatalf("trusted_proxies = %q, want %q from the --config file", cfg.TrustedProxies, want)
	}
}

// With the default config no origin gets CORS access: a cross-origin page can
// neither read API responses nor pass a preflight.
func TestDefaultCORSAllowsNoOrigins(t *testing.T) {
	cfg := Default()
	if len(cfg.CORS.AllowOrigins) != 0 {
		t.Fatalf("default cors.allow_origins = %q, want none", cfg.CORS.AllowOrigins)
	}
	h := middleware.CORS(cfg.CORS)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		r := httptest.NewRequest(method, "/api/v1/users", nil)
		r.Header.Set("Origin", "https://other.example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want none", method, got)
		}
	}
}

func TestLoadCookieSecure(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		set       bool
		want      string
		wantErr   bool
	}{
		{name: "default", want: "auto"},
		{name: "auto", env: "auto", set: true, want: "auto"},
		{name: "always", env: "always", set: true, want: "always"},
		{name: "unknown value", env: "never", set: true, wantErr: true},
		{name: "case matters", env: "Always", set: true, wantErr: true},
		{name: "empty", env: "", set: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			if tc.set {
				t.Setenv("SOLTI_AUTH_COOKIE_SECURE", tc.env)
			}
			cfg, err := load("")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "config.auth") {
					t.Fatalf("want a config.auth error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Auth.CookieSecure != tc.want {
				t.Fatalf("auth.cookie_secure = %q, want %q", cfg.Auth.CookieSecure, tc.want)
			}
		})
	}
}

func TestLoadRejectsMalformedCORSOrigins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("cors:\n  allow_origins:\n    - https://app.example.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	if _, err := load(""); err == nil || !strings.Contains(err.Error(), "config.cors") {
		t.Fatalf("origin with a path: want a config.cors error, got %v", err)
	}
}

// The first administrator's password comes from the environment only; a YAML
// file that holds it fails loading (TestEnvOnlyKeyInFileFails).
func TestLoadBootstrapAdminPasswordFromTheEnvironment(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv(bootstrap.AdminPasswordEnv, "from-the-environment")
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapAdminPassword != "from-the-environment" {
		t.Fatalf("bootstrap admin password = %q, want the environment value", cfg.BootstrapAdminPassword)
	}
}

// The enrollment secret comes from the environment only; a YAML file that holds
// it fails loading (TestEnvOnlyKeyInFileFails).
func TestLoadEnrollSecretFromTheEnvironment(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", "from-the-environment-0123456789abcdef")
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentAuth.EnrollSecret != "from-the-environment-0123456789abcdef" {
		t.Fatal("the environment did not set the enrollment secret")
	}
}

// A secret under 32 bytes fails configuration loading with an error that names
// the setting and never shows the value. Unset or empty means no secret.
func TestEnrollSecretTooShortFailsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		wantErr      bool
	}{
		{name: "empty", secret: ""},
		{name: "31 bytes", secret: strings.Repeat("s", 31), wantErr: true},
		{name: "32 bytes", secret: strings.Repeat("s", 32)},
		{name: "32 bytes in 16 characters", secret: strings.Repeat("é", 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", tc.secret)
			_, err := load("")
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrEnrollSecretTooShort) || !strings.Contains(err.Error(), "config.agent_auth") {
				t.Fatalf("want a config.agent_auth error wrapping ErrEnrollSecretTooShort, got %v", err)
			}
			if !strings.Contains(err.Error(), "SOLTI_AGENT_AUTH_ENROLL_SECRET") || strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("error %q must name the variable and not show the value", err)
			}
		})
	}
}

// The secret closes enrollment only while agent auth is required. Set with
// require false it would do nothing, so loading fails instead of letting an
// operator believe discovery is closed.
func TestEnrollSecretNeedsAgentAuthRequired(t *testing.T) {
	requireOff := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(requireOff, []byte("agent_auth:\n  require: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, require string
		secret, wantErr     bool
	}{
		{name: "required by default", secret: true},
		{name: "required explicitly", require: "true", secret: true},
		{name: "env turns it off", require: "false", secret: true, wantErr: true},
		{name: "yaml turns it off", path: requireOff, secret: true, wantErr: true},
		{name: "off without a secret", require: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", tc.path)
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			secret := ""
			if tc.secret {
				secret = strings.Repeat("s", 32)
			}
			t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", secret)
			if tc.require != "" {
				t.Setenv("SOLTI_AGENT_AUTH_REQUIRE", tc.require)
			}
			_, err := load("")
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrEnrollSecretNeedsRequire) || !strings.Contains(err.Error(), "config.agent_auth") {
				t.Fatalf("want a config.agent_auth error wrapping ErrEnrollSecretNeedsRequire, got %v", err)
			}
			if !strings.Contains(err.Error(), "agent_auth.require") || strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q must name agent_auth.require and not show the value", err)
			}
		})
	}
}

// The raft cluster of the README's "Cluster" example loads from YAML and from
// the environment, whose comma-separated peers may have spaces after the
// commas, and so does plain TCP allowed from the environment. A raft
// configuration that fails cluster validation fails loading.
func TestLoadRaftCluster(t *testing.T) {
	const example = `cluster:
  backend: raft
  raft:
    node_id: n1
    bind_addr: 0.0.0.0:7000
    advertise_addr: 10.0.0.1:7000
    data_dir: /var/lib/solti/raft
    election_timeout_ms: 1000
    heartbeat_timeout_ms: 500
    snapshot_interval: 30s
    snapshot_threshold: 4096
    trailing_logs: 2048
    tls:
      cert_file: /etc/solti/raft/n1.crt
      key_file: /etc/solti/raft/n1.key
      ca_file: /etc/solti/raft/ca.crt
  discovery:
    driver: static
    peers:
      - n1=10.0.0.1:7000
      - n2=10.0.0.2:7000
      - n3=10.0.0.3:7000
`
	yamlPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	static := map[string]string{
		"SOLTI_CLUSTER_BACKEND":                   "raft",
		"SOLTI_CLUSTER_RAFT_NODE_ID":              "n1",
		"SOLTI_CLUSTER_RAFT_BIND_ADDR":            "0.0.0.0:7000",
		"SOLTI_CLUSTER_RAFT_ADVERTISE_ADDR":       "10.0.0.1:7000",
		"SOLTI_CLUSTER_RAFT_DATA_DIR":             "/var/lib/solti/raft",
		"SOLTI_CLUSTER_RAFT_ELECTION_TIMEOUT_MS":  "1000",
		"SOLTI_CLUSTER_RAFT_HEARTBEAT_TIMEOUT_MS": "500",
		"SOLTI_CLUSTER_RAFT_SNAPSHOT_INTERVAL":    "30s",
		"SOLTI_CLUSTER_RAFT_SNAPSHOT_THRESHOLD":   "4096",
		"SOLTI_CLUSTER_RAFT_TRAILING_LOGS":        "2048",
		"SOLTI_CLUSTER_RAFT_TLS_CERT_FILE":        "/etc/solti/raft/n1.crt",
		"SOLTI_CLUSTER_RAFT_TLS_KEY_FILE":         "/etc/solti/raft/n1.key",
		"SOLTI_CLUSTER_RAFT_TLS_CA_FILE":          "/etc/solti/raft/ca.crt",
		"SOLTI_CLUSTER_DISCOVERY_DRIVER":          "static",
		"SOLTI_CLUSTER_DISCOVERY_PEERS":           "n1=10.0.0.1:7000, n2=10.0.0.2:7000, n3=10.0.0.3:7000",
	}
	// plainTCP is static with the flag instead of the TLS files.
	plainTCP := map[string]string{"SOLTI_CLUSTER_RAFT_ALLOW_INSECURE_TRANSPORT": "true"}
	for key, value := range static {
		if !strings.HasPrefix(key, "SOLTI_CLUSTER_RAFT_TLS_") {
			plainTCP[key] = value
		}
	}
	raft := cluster.RaftConfig{
		NodeID:             "n1",
		BindAddr:           "0.0.0.0:7000",
		AdvertiseAddr:      "10.0.0.1:7000",
		DataDir:            "/var/lib/solti/raft",
		ElectionTimeoutMs:  1000,
		HeartbeatTimeoutMs: 500,
		SnapshotInterval:   30 * time.Second,
		SnapshotThreshold:  4096,
		TrailingLogs:       2048,
		TLS: cluster.RaftTLSConfig{
			CertFile: "/etc/solti/raft/n1.crt",
			KeyFile:  "/etc/solti/raft/n1.key",
			CAFile:   "/etc/solti/raft/ca.crt",
		},
	}
	insecure := raft
	insecure.TLS, insecure.AllowInsecureTransport = cluster.RaftTLSConfig{}, true
	// withoutCompaction leaves out the compaction settings, which then load as
	// the defaults.
	withoutCompaction := map[string]string{}
	for key, value := range static {
		if !strings.Contains(key, "_SNAPSHOT_") && !strings.HasSuffix(key, "_TRAILING_LOGS") {
			withoutCompaction[key] = value
		}
	}
	defaults := raft
	defaults.SnapshotInterval = cluster.DefaultRaftSnapshotInterval
	defaults.SnapshotThreshold = cluster.DefaultRaftSnapshotThreshold
	defaults.TrailingLogs = cluster.DefaultRaftTrailingLogs
	peers := []cluster.Peer{{ID: "n1", Address: "10.0.0.1:7000"}, {ID: "n2", Address: "10.0.0.2:7000"}, {ID: "n3", Address: "10.0.0.3:7000"}}
	for _, tc := range []struct {
		name, path string
		env        map[string]string
		raft       cluster.RaftConfig // what loads, with the peers, when want is nil
		want       error
	}{
		{name: "yaml", path: yamlPath, raft: raft},
		{name: "environment", env: static, raft: raft},
		{name: "compaction defaults", env: withoutCompaction, raft: defaults},
		{name: "plain TCP allowed", env: plainTCP, raft: insecure},
		{name: "refused", env: map[string]string{
			"SOLTI_CLUSTER_BACKEND":                       "raft",
			"SOLTI_CLUSTER_RAFT_NODE_ID":                  "n1",
			"SOLTI_CLUSTER_RAFT_BIND_ADDR":                "10.0.0.1:7000",
			"SOLTI_CLUSTER_RAFT_DATA_DIR":                 "/var/lib/solti/raft",
			"SOLTI_CLUSTER_RAFT_ALLOW_INSECURE_TRANSPORT": "true",
			"SOLTI_CLUSTER_DISCOVERY_PEERS":               "10.0.0.1:7000,10.0.0.2:7000,10.0.0.3:7000",
		}, want: cluster.ErrNodeIDNotAdvertise},
		{name: "no transport security", env: map[string]string{
			"SOLTI_CLUSTER_BACKEND":         "raft",
			"SOLTI_CLUSTER_RAFT_NODE_ID":    "n1",
			"SOLTI_CLUSTER_RAFT_BIND_ADDR":  "10.0.0.1:7000",
			"SOLTI_CLUSTER_RAFT_DATA_DIR":   "/var/lib/solti/raft",
			"SOLTI_CLUSTER_DISCOVERY_PEERS": "n1=10.0.0.1:7000",
		}, want: cluster.ErrRaftTLSRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", tc.path)
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			cfg, err := load("")
			if tc.want != nil {
				if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "config.cluster") {
					t.Fatalf("want a config.cluster error wrapping %v, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Cluster.Backend != cluster.BackendRaft || cfg.Cluster.Raft != tc.raft {
				t.Fatalf("cluster = %+v, want backend raft with %+v", cfg.Cluster, tc.raft)
			}
			if got, err := cfg.Cluster.Discovery.StaticPeers(); err != nil || !slices.Equal(got, peers) {
				t.Fatalf("peers = %v, %v; want %v", got, err, peers)
			}
		})
	}
}

// The DNS settings load from the environment as well.
func TestLoadRaftClusterWithDNS(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	for key, value := range map[string]string{
		"SOLTI_CLUSTER_BACKEND":            "raft",
		"SOLTI_CLUSTER_RAFT_NODE_ID":       "10.0.0.1:7000",
		"SOLTI_CLUSTER_RAFT_BIND_ADDR":     "10.0.0.1:7000",
		"SOLTI_CLUSTER_RAFT_DATA_DIR":      "/var/lib/solti/raft",
		"SOLTI_CLUSTER_RAFT_TLS_CERT_FILE": "/etc/solti/raft/node.crt",
		"SOLTI_CLUSTER_RAFT_TLS_KEY_FILE":  "/etc/solti/raft/node.key",
		"SOLTI_CLUSTER_RAFT_TLS_CA_FILE":   "/etc/solti/raft/ca.crt",
		"SOLTI_CLUSTER_DISCOVERY_DRIVER":   "dns",
		"SOLTI_CLUSTER_DISCOVERY_HOSTNAME": "podium.svc",
		"SOLTI_CLUSTER_DISCOVERY_PORT":     "7000",
	} {
		t.Setenv(key, value)
	}
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	want := cluster.DiscoveryConfig{Driver: cluster.DriverDNS, Hostname: "podium.svc", Port: 7000}
	if d := cfg.Cluster.Discovery; d.Driver != want.Driver || d.Hostname != want.Hostname || d.Port != want.Port {
		t.Fatalf("discovery = %+v, want %+v", d, want)
	}
}

// The log level defaults to info and the format to json; the file and the
// environment set them, the environment last.
func TestLoadLogSettings(t *testing.T) {
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("CONFIG_PATH", "")
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Fatalf("default log settings = %+v, want info and json", cfg.Log)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: warn\n  format: text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load(path); err != nil || cfg.Log.Level != "warn" || cfg.Log.Format != "text" {
		t.Fatalf("from the file: %+v, %v; want warn and text", cfg.Log, err)
	}
	t.Setenv("SOLTI_LOG_LEVEL", "debug")
	t.Setenv("SOLTI_LOG_FORMAT", "json")
	if cfg, err = load(path); err != nil || cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Fatalf("from the environment: %+v, %v; want debug and json over the file", cfg.Log, err)
	}
}

// A level or a format that is not one of the four and the two fails loading,
// whichever way it is set.
func TestLoadRejectsAnInvalidLogSetting(t *testing.T) {
	for _, tc := range []struct {
		name, env, value string
		want             error
	}{
		{"unknown level", "SOLTI_LOG_LEVEL", "verbose", ErrInvalidLogLevel},
		{"trace is no level", "SOLTI_LOG_LEVEL", "trace", ErrInvalidLogLevel},
		{"level in capitals", "SOLTI_LOG_LEVEL", "INFO", ErrInvalidLogLevel},
		{"empty level", "SOLTI_LOG_LEVEL", "", ErrInvalidLogLevel},
		{"unknown format", "SOLTI_LOG_FORMAT", "xml", ErrInvalidLogFormat},
		{"empty format", "SOLTI_LOG_FORMAT", "", ErrInvalidLogFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv("CONFIG_PATH", "")
			t.Setenv(tc.env, tc.value)
			_, err := load("")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "config.log") {
				t.Fatalf("load = %v, want %v under config.log", err, tc.want)
			}
		})
	}
}

// The ops listener defaults to loopback port 8081; the file and the environment
// set it, and an empty ops.addr switches the listener off, whichever way it is
// set.
func TestLoadOpsSettings(t *testing.T) {
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("CONFIG_PATH", "")
	cfg, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ops.Addr != "127.0.0.1:8081" || cfg.Ops.Name != "ops" {
		t.Fatalf("default ops settings = %+v, want 127.0.0.1:8081 and ops", cfg.Ops)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ops:\n  addr: 10.0.0.5:9000\n  name: probes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load(path); err != nil || cfg.Ops.Addr != "10.0.0.5:9000" || cfg.Ops.Name != "probes" {
		t.Fatalf("from the file: %+v, %v", cfg.Ops, err)
	}
	t.Setenv("SOLTI_OPS_ADDR", "")
	if cfg, err = load(path); err != nil || cfg.Ops.Enabled() {
		t.Fatalf("from the environment: %+v, %v; want the listener off over the file", cfg.Ops, err)
	}
}
