package cluster_test

import (
	"testing"

	"github.com/kelseyhightower/envconfig"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"gopkg.in/yaml.v3"
)

func TestStandaloneDataDirectoryConfig(t *testing.T) {
	cfg := cluster.DefaultConfig()
	if cfg.Backend != cluster.BackendStandalone || cfg.Standalone.DataDir != cluster.DefaultStandaloneDataDir {
		t.Fatal("durable default missing")
	}
	if err := yaml.Unmarshal([]byte("standalone:\n  data_dir: /var/lib/solti/podium\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Standalone.DataDir != "/var/lib/solti/podium" {
		t.Fatal("YAML path not loaded")
	}
	t.Setenv("SOLTI_CLUSTER_STANDALONE_DATA_DIR", "/tmp/solti-config-fixture")
	if err := envconfig.Process("SOLTI_CLUSTER", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Standalone.DataDir != "/tmp/solti-config-fixture" {
		t.Fatal("environment path not loaded")
	}
}

func TestStandaloneDurabilityMustBeExplicit(t *testing.T) {
	cfg := cluster.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Standalone.DataDir = " "
	if err := cfg.Validate(); err == nil {
		t.Fatal("blank durable path accepted")
	}
	cfg.Standalone.Ephemeral = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOLTI_CLUSTER_STANDALONE_EPHEMERAL", "true")
	cfg = cluster.DefaultConfig()
	if err := envconfig.Process("SOLTI_CLUSTER", &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Standalone.Ephemeral {
		t.Fatal("explicit ephemeral environment option not loaded")
	}
}

func TestDataDirIsThatOfTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   string
	}{
		{"standalone", func(*cluster.Config) {}, "/var/lib/solti/podium"},
		{"unset backend is standalone", func(c *cluster.Config) { c.Backend = "" }, "/var/lib/solti/podium"},
		{"ephemeral standalone has no directory", func(c *cluster.Config) { c.Standalone.Ephemeral = true }, ""},
		{"raft", func(c *cluster.Config) { c.Backend = cluster.BackendRaft }, "/var/lib/solti/raft"},
		{"ephemeral is a standalone option", func(c *cluster.Config) {
			c.Backend = cluster.BackendRaft
			c.Standalone.Ephemeral = true
		}, "/var/lib/solti/raft"},
		{"unknown backend", func(c *cluster.Config) { c.Backend = "other" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			cfg.Standalone.DataDir = "/var/lib/solti/podium"
			cfg.Raft.DataDir = "/var/lib/solti/raft"
			tc.mutate(&cfg)
			if got := cfg.DataDir(); got != tc.want {
				t.Fatalf("DataDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEphemeralIsAStandaloneOption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Config)
		want   bool
	}{
		{"durable standalone", func(*cluster.Config) {}, false},
		{"ephemeral standalone", func(c *cluster.Config) { c.Standalone.Ephemeral = true }, true},
		{"ephemeral with an unset backend", func(c *cluster.Config) {
			c.Backend = ""
			c.Standalone.Ephemeral = true
		}, true},
		{"raft ignores the flag", func(c *cluster.Config) {
			c.Backend = cluster.BackendRaft
			c.Standalone.Ephemeral = true
		}, false},
		{"unknown backend", func(c *cluster.Config) {
			c.Backend = "other"
			c.Standalone.Ephemeral = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cluster.DefaultConfig()
			tc.mutate(&cfg)
			if got := cfg.Ephemeral(); got != tc.want {
				t.Fatalf("Ephemeral() = %v, want %v", got, tc.want)
			}
		})
	}
}
