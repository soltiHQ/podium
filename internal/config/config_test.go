package config

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/soltiHQ/control-plane/internal/bootstrap"
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
			cfg, err := load(nil)
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

func TestLoadReadsTheConfigFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("trusted_proxies:\n  - 10.0.0.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	cfg, err := load([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.9"}; !slices.Equal(cfg.TrustedProxies, want) {
		t.Fatalf("trusted_proxies = %q, want %q from the --config file", cfg.TrustedProxies, want)
	}
}

// A mistyped flag is still reported on stderr; the path then comes from CONFIG_PATH.
func TestMistypedConfigFlagIsReported(t *testing.T) {
	t.Setenv("CONFIG_PATH", "from-env.yaml")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	stderr := os.Stderr
	os.Stderr = w
	path := configPath([]string{"--confg", "typo.yaml"})
	os.Stderr = stderr
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "flag provided but not defined: -confg") {
		t.Fatalf("stderr = %q, want the unknown flag reported", out)
	}
	if path != "from-env.yaml" {
		t.Fatalf("config path = %q, want CONFIG_PATH", path)
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
			cfg, err := load(nil)
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
	if _, err := load(nil); err == nil || !strings.Contains(err.Error(), "config.cors") {
		t.Fatalf("origin with a path: want a config.cors error, got %v", err)
	}
}

// The first administrator's password comes from the environment only: a YAML
// file cannot carry it.
func TestLoadBootstrapAdminPasswordIsEnvOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// bootstrapadminpassword is the key yaml.v3 derives when the field has no tag.
	body := "bootstrapadminpassword: from-yaml-file\nbootstrap_admin_password: from-yaml-file\nbootstrap:\n  admin_password: from-yaml-file\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	cfg, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapAdminPassword != "" {
		t.Fatal("the YAML file set the bootstrap admin password")
	}
	t.Setenv(bootstrap.AdminPasswordEnv, "from-the-environment")
	cfg, err = load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapAdminPassword != "from-the-environment" {
		t.Fatalf("bootstrap admin password = %q, want the environment value", cfg.BootstrapAdminPassword)
	}
}

// The enrollment secret comes from the environment only: a YAML file cannot
// carry it, under the key it would naturally have or under the one yaml.v3
// derives when the field has no tag.
func TestLoadEnrollSecretIsEnvOnly(t *testing.T) {
	secret := strings.Repeat("y", 32)
	for _, key := range []string{
		"enroll_secret",
		"enrollsecret", // the key yaml.v3 derives when the field has no tag
	} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("agent_auth:\n  "+key+": "+secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CONFIG_PATH", path)
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			cfg, err := load(nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AgentAuth.EnrollSecret != "" {
				t.Fatalf("the YAML file set the enrollment secret through agent_auth.%s", key)
			}
		})
	}

	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", "from-the-environment-0123456789abcdef")
	cfg, err := load(nil)
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
			_, err := load(nil)
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
			_, err := load(nil)
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
