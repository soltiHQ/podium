package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
			cfg, err := Load()
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
