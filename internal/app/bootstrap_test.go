package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
)

// Bootstrap writes the generated password where the selected backend keeps its
// state, logs it only when that state is explicitly ephemeral, and gets the
// password the operator chose. A backend that is neither stays closed: no data
// directory and no ephemeral flag.
func TestBootstrapOptionsFollowTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*config.Config)
		dataDir   string
		ephemeral bool
	}{
		{"standalone", func(*config.Config) {}, "/data/standalone", false},
		{"ephemeral standalone", func(c *config.Config) { c.Cluster.Standalone.Ephemeral = true }, "", true},
		{"raft", func(c *config.Config) { c.Cluster.Backend = cluster.BackendRaft }, "/data/raft", false},
		{"raft ignores the standalone flag", func(c *config.Config) {
			c.Cluster.Backend = cluster.BackendRaft
			c.Cluster.Standalone.Ephemeral = true
		}, "/data/raft", false},
		{"unknown backend", func(c *config.Config) {
			c.Cluster.Backend = "other"
			c.Cluster.Standalone.Ephemeral = true
		}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Cluster.Standalone.DataDir = "/data/standalone"
			cfg.Cluster.Raft.DataDir = "/data/raft"
			cfg.BootstrapAdminPassword = "operator-chosen-password"
			tc.mutate(&cfg)
			want := bootstrap.Options{DataDir: tc.dataDir, Ephemeral: tc.ephemeral, AdminPassword: "operator-chosen-password"}
			if got := bootstrapOptions(cfg); got != want {
				t.Fatalf("bootstrapOptions = %+v, want %+v", got, want)
			}
		})
	}
}

// A fresh install with the default configuration starts and writes its
// administrator password into the data directory, without logging it. A
// restart leaves that file alone.
func TestNewWritesAdminPasswordFileOnFirstStart(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.Standalone.DataDir = filepath.Join(t.TempDir(), "podium")
	cfg.Auth.JWTSecret = strings.Repeat("first-start-test-secret", 2)
	path := filepath.Join(cfg.Cluster.Standalone.DataDir, bootstrap.PasswordFile)

	start := func() string {
		t.Helper()
		var logs bytes.Buffer
		a, err := New(context.Background(), cfg, zerolog.New(&logs))
		if err != nil {
			t.Fatal(err)
		}
		if err = a.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		return logs.String()
	}

	first := start()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the first start wrote no password file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file has mode %v, want 0600", info.Mode().Perm())
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(written))
	if password == "" {
		t.Fatal("password file is empty")
	}
	if strings.Contains(first, password) {
		t.Fatalf("the log contains the password:\n%s", first)
	}
	if !strings.Contains(first, path) {
		t.Fatalf("the log does not name %s:\n%s", path, first)
	}

	if second := start(); strings.Contains(second, password) {
		t.Fatalf("the restart log contains the password:\n%s", second)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, again) {
		t.Fatal("a restart rewrote the password file")
	}
}

// raftSeedConfig is a raft configuration that passes validation, for node n1 of
// three, whose TLS files and data directory do not exist: the node fails if it
// ever starts.
func raftSeedConfig(t *testing.T) (cfg config.Config, dir string) {
	t.Helper()
	cfg = config.Default()
	cfg.Cluster, dir = unopenedRaftConfig(t)
	cfg.Auth.JWTSecret = strings.Repeat("raft-seed-check-secret", 2)
	return cfg, dir
}

// A raft node seeds at a leadership term, which can come at any time, so New
// checks what any seed needs before anything starts. A password from
// SOLTI_BOOTSTRAP_ADMIN_PASSWORD that breaks the password policy fails New,
// whatever the user table holds, and the node never starts: its data
// directory is not created. Neither the error nor the log has the value.
func TestNewRaftRefusesAnAdminPasswordBreakingThePolicy(t *testing.T) {
	for _, tc := range []struct{ name, password string }{
		{"too short", "Sh0rt-pw-11"},
		{"too long", strings.Repeat("long-password-", 6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dir := raftSeedConfig(t)
			cfg.BootstrapAdminPassword = tc.password
			var logs bytes.Buffer
			a, err := New(context.Background(), cfg, zerolog.New(&logs))
			if err == nil {
				_ = a.Shutdown(context.Background())
				t.Fatal("New accepted the password")
			}
			if !errors.Is(err, auth.ErrWeakPassword) || !strings.Contains(err.Error(), bootstrap.AdminPasswordEnv) {
				t.Fatalf("New() = %v, want ErrWeakPassword naming %s", err, bootstrap.AdminPasswordEnv)
			}
			if strings.Contains(err.Error(), tc.password) || strings.Contains(logs.String(), tc.password) {
				t.Fatal("the refused password was echoed")
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the data directory exists after the refusal: %v", err)
			}
		})
	}
}

// A raft node without a data directory, which a configuration that config.Load
// validated always has, could not write a generated password down: New fails
// with ErrNoDataDir before anything starts, ahead of the cluster's own check of
// the same setting.
func TestNewRaftRefusesToStartWithoutADataDirectory(t *testing.T) {
	cfg, _ := raftSeedConfig(t)
	cfg.Cluster.Raft.DataDir = ""
	a, err := New(context.Background(), cfg, zerolog.Nop())
	if err == nil {
		_ = a.Shutdown(context.Background())
		t.Fatal("New started a raft node without a data directory")
	}
	if !errors.Is(err, bootstrap.ErrNoDataDir) {
		t.Fatalf("New() = %v, want ErrNoDataDir", err)
	}
}
