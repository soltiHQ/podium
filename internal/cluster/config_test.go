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
