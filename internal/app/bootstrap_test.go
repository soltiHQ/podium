package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
)

// Bootstrap writes the generated password where the selected backend keeps its
// state, and gets the password the operator chose.
func TestBootstrapOptionsFollowTheSelectedBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*config.Config)
		dataDir string
	}{
		{"standalone", func(*config.Config) {}, "/data/standalone"},
		{"ephemeral standalone", func(c *config.Config) { c.Cluster.Standalone.Ephemeral = true }, ""},
		{"raft", func(c *config.Config) { c.Cluster.Backend = cluster.BackendRaft }, "/data/raft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Cluster.Standalone.DataDir = "/data/standalone"
			cfg.Cluster.Raft.DataDir = "/data/raft"
			cfg.BootstrapAdminPassword = "operator-chosen-password"
			tc.mutate(&cfg)
			want := bootstrap.Options{DataDir: tc.dataDir, AdminPassword: "operator-chosen-password"}
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
