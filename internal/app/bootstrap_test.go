package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth"
	authcred "github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/bootstrap"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"golang.org/x/crypto/bcrypt"
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
		{"memory", func(*config.Config) {}, "", true},
		{"unset backend is memory", func(c *config.Config) { c.Cluster.Backend = "" }, "", true},
		{"raft", func(c *config.Config) { c.Cluster.Backend = cluster.BackendRaft }, "/data/raft", false},
		{"unknown backend", func(c *config.Config) { c.Cluster.Backend = "other" }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
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

// A fresh install with the default configuration, the memory backend, starts and
// logs its generated administrator password once, at WARN, and writes no file:
// the state is in memory, so there is no data directory for it.
func TestNewLogsAdminPasswordOnFirstMemoryStart(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("first-start-test-secret", 2)
	dir := t.TempDir()
	t.Chdir(dir)

	var logs bytes.Buffer
	a, err := New(context.Background(), cfg, zerolog.New(&logs), WithPasswordCost(bcrypt.MinCost))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if n := strings.Count(out, "admin password generated"); n != 1 || !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, `"password":"`) {
		t.Fatalf("the log holds %d lines about the generated password, want one WARN with the password:\n%s", n, out)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the start wrote %v (%v) into its working directory, want nothing", entries, err)
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
			a, err := New(context.Background(), cfg, zerolog.New(&logs), WithPasswordCost(bcrypt.MinCost))
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
	a, err := New(context.Background(), cfg, zerolog.Nop(), WithPasswordCost(bcrypt.MinCost))
	if err == nil {
		_ = a.Shutdown(context.Background())
		t.Fatal("New started a raft node without a data directory")
	}
	if !errors.Is(err, bootstrap.ErrNoDataDir) {
		t.Fatalf("New() = %v, want ErrNoDataDir", err)
	}
}

// The cost New is given is the cost of the password hashes the services write:
// the hash of the administrator that a memory start seeds has it. A New that
// dropped the option, or passed the services another cost, would hash at the
// default, 12.
func TestPasswordCostReachesTheServices(t *testing.T) {
	const cost = bcrypt.MinCost + 1 // neither the default nor the floor
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("password-cost-test-secret", 2)
	a, err := New(context.Background(), cfg, zerolog.Nop(), WithPasswordCost(cost))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	ctx := context.Background()
	admin, err := a.store.GetUserBySubject(ctx, "admin")
	if err != nil {
		t.Fatalf("the seeded administrator: %v", err)
	}
	cred, err := a.store.GetCredentialByUserAndAuth(ctx, admin.ID(), enum.Password)
	if err != nil {
		t.Fatalf("the administrator's password credential: %v", err)
	}
	verifier, err := a.store.GetVerifierByCredential(ctx, cred.ID())
	if err != nil {
		t.Fatalf("the administrator's password verifier: %v", err)
	}
	hash, ok := verifier.DataGet(authcred.PasswordHashKey)
	if !ok {
		t.Fatal("the administrator's verifier holds no password hash")
	}
	if got, err := bcrypt.Cost([]byte(hash)); err != nil || got != cost {
		t.Fatalf("the stored hash has cost %d (%v), want the cost New was given, %d", got, err, cost)
	}
}

// A production start hashes passwords at cost 12; only an option, which tests
// use, lowers it.
func TestDefaultPasswordCostIsTwelve(t *testing.T) {
	if got := defaultOptions().passwordCost; got != 12 {
		t.Fatalf("default password cost = %d, want 12", got)
	}
	o := defaultOptions()
	WithPasswordCost(bcrypt.MinCost)(&o)
	if o.passwordCost != bcrypt.MinCost {
		t.Fatalf("WithPasswordCost did not apply: %d", o.passwordCost)
	}
}
