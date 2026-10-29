package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/role"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func TestRestartPreservesAdminAndPassword(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	roles, users, creds := role.New(store, logger), user.New(store, logger), credential.New(store, logger)
	if err := reconcileRoles(ctx, logger, roles); err != nil {
		t.Fatal(err)
	}
	u, err := model.NewUser(adminUserID, adminSubject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetName("Operator's edited profile")
	u.SetEmail("operator@example.test")
	u.SetRoleIDs([]string{enum.RoleAdminID})
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := creds.SetPassword(ctx, credential.SetPasswordRequest{UserID: u.ID(), Password: "existing-operator-password", Cost: 4}); err != nil {
		t.Fatal(err)
	}
	u.Disable()
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetVerifierByCredential(ctx, "cred-"+adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Run(ctx, logger, Options{cost: bcrypt.MinCost}, roles, users, creds); err != nil {
			t.Fatal(err)
		}
	}
	after, err := users.Get(ctx, adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name() != u.Name() || after.Email() != u.Email() || !after.Disabled() || !after.UpdatedAt().Equal(u.UpdatedAt()) {
		t.Fatal("restart replaced existing admin profile")
	}
	verifier, err := store.GetVerifierByCredential(ctx, "cred-"+adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, _ := before.DataGet(credentials.PasswordHashKey)
	afterHash, _ := verifier.DataGet(credentials.PasswordHashKey)
	if beforeHash != afterHash {
		t.Fatal("restart replaced existing password")
	}
	c, err := store.GetCredentialByUserAndAuth(ctx, adminUserID, enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.VerifyPassword(c, verifier, "existing-operator-password"); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapSeedsOnceAndRepairsPartialFirstStart(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	roles, users, creds := role.New(store, logger), user.New(store, logger), credential.New(store, logger)
	if err := reconcileRoles(ctx, logger, roles); err != nil {
		t.Fatal(err)
	}
	u, err := model.NewUser(adminUserID, adminSubject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetName("partial initial start")
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, logger, Options{cost: bcrypt.MinCost}, roles, users, creds); err != nil {
		t.Fatal(err)
	}
	cred, err := store.GetCredentialByUserAndAuth(ctx, adminUserID, enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetVerifierByCredential(ctx, cred.ID()); err != nil {
		t.Fatal(err)
	}
	got, err := users.Get(ctx, adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != u.Name() {
		t.Fatal("partial repair replaced profile")
	}
	roleBefore, err := roles.Get(ctx, enum.RoleAdminID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, logger, Options{cost: bcrypt.MinCost}, roles, users, creds); err != nil {
		t.Fatal(err)
	}
	roleAfter, err := roles.Get(ctx, enum.RoleAdminID)
	if err != nil {
		t.Fatal(err)
	}
	if !roleBefore.UpdatedAt().Equal(roleAfter.UpdatedAt()) {
		t.Fatal("restart recreated role")
	}
}

func TestCancelledBootstrapDoesNotSeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := inmemory.New()
	logger := zerolog.New(io.Discard)
	err := Run(ctx, logger, Options{}, role.New(store, logger), user.New(store, logger), credential.New(store, logger))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := store.GetUser(context.Background(), adminUserID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unexpected user: %v", err)
	}
}

// fixture is the services bootstrap works on, over one in-memory store, and a
// log that keeps the entries of every level.
type fixture struct {
	store  *inmemory.Store
	logs   *bytes.Buffer
	logger zerolog.Logger
	roles  *role.Service
	users  *user.Service
	creds  *credential.Service
}

func newFixture() *fixture {
	var (
		store  = inmemory.New()
		logs   = new(bytes.Buffer)
		logger = zerolog.New(logs)
	)
	return &fixture{
		store:  store,
		logs:   logs,
		logger: logger,
		roles:  role.New(store, logger),
		users:  user.New(store, logger),
		creds:  credential.New(store, logger),
	}
}

// run is [Run] with a cheap bcrypt cost.
func (f *fixture) run(opts Options) error {
	opts.cost = bcrypt.MinCost
	return Run(context.Background(), f.logger, opts, f.roles, f.users, f.creds)
}

// entry is a log line as the tests read it.
type entry struct {
	Level    string `json:"level"`
	Message  string `json:"message"`
	Path     string `json:"path"`
	Password string `json:"password"`
}

func (f *fixture) entries(t *testing.T) []entry {
	t.Helper()
	var out []entry
	for _, line := range bytes.Split(bytes.TrimSpace(f.logs.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// ignoredWarnings counts the WARN entries that say the password variable is ignored.
func (f *fixture) ignoredWarnings(t *testing.T) int {
	t.Helper()
	n := 0
	for _, e := range f.entries(t) {
		if e.Level == "warn" && strings.Contains(e.Message, AdminPasswordEnv) {
			n++
		}
	}
	return n
}

// filePassword reads the generated password from the file bootstrap wrote in dir.
func filePassword(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, PasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// names lists what dir holds.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// login logs in as the administrator through the real login service and
// returns the permissions the session got.
func login(t *testing.T, store storage.Storage, password string) []enum.Permission {
	t.Helper()
	cfg := auth.Config{JWTSecret: strings.Repeat("k", auth.MinJWTSecretLen), CookieSecure: auth.CookieSecureAuto}
	_, id, err := kit.New(store, cfg).Session.Login(context.Background(), enum.Password, adminSubject, password)
	if err != nil {
		t.Fatalf("login as %s: %v", adminSubject, err)
	}
	return id.Permissions
}

// passwordWorks reports whether password matches the stored administrator password.
func passwordWorks(t *testing.T, store storage.Storage, password string) bool {
	t.Helper()
	ctx := context.Background()
	c, err := store.GetCredentialByUserAndAuth(ctx, adminUserID, enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.GetVerifierByCredential(ctx, c.ID())
	if err != nil {
		t.Fatal(err)
	}
	return credentials.VerifyPassword(c, v, password) == nil
}

// The first start writes the generated password to the data directory, and
// that password logs in. A restart leaves the file and the password alone.
func TestBootstrapWritesPasswordFileOnce(t *testing.T) {
	f, dir := newFixture(), t.TempDir()
	if err := f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PasswordFile)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("password file is %v, want a regular file with mode 0600", info.Mode())
	}
	password := filePassword(t, dir)
	if got := login(t, f.store, password); len(got) != len(enum.All) {
		t.Fatalf("the administrator got %d permissions, want all %d", len(got), len(enum.All))
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a restart rewrote the password file")
	}
	login(t, f.store, password)
	if got := names(t, dir); !slices.Equal(got, []string{PasswordFile}) {
		t.Fatalf("data directory holds %q, want only the password file", got)
	}
}

// A file left by an earlier install is replaced, whole, when a fresh store is seeded.
func TestBootstrapReplacesAStalePasswordFile(t *testing.T) {
	f, dir := newFixture(), t.TempDir()
	stale := "stale-password-of-an-old-install"
	path := filepath.Join(dir, PasswordFile)
	if err := os.WriteFile(path, []byte(stale+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // whatever the umask says
		t.Fatal(err)
	}
	if err := f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replaced file has mode %v, want 0600", info.Mode().Perm())
	}
	password := filePassword(t, dir)
	if password == stale {
		t.Fatal("the stale password file was kept")
	}
	login(t, f.store, password)
}

// An operator who replaced user-admin by another administrator keeps it that
// way: a restart creates neither the user nor its password again.
func TestBootstrapDoesNotResurrectDeletedAdmin(t *testing.T) {
	ctx := context.Background()
	f, dir := newFixture(), t.TempDir()
	if err := f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	ops, err := model.NewUser("ops", "ops")
	if err != nil {
		t.Fatal(err)
	}
	ops.SetRoleIDs([]string{enum.RoleAdminID})
	if err = f.users.Upsert(ctx, ops); err != nil {
		t.Fatal(err)
	}
	if err = f.users.Delete(ctx, &identity.Identity{UserID: ops.ID(), Permissions: enum.All}, adminUserID); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, PasswordFile))
	if err != nil {
		t.Error(err)
	}

	if err = f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.GetUser(ctx, adminUserID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("user-admin came back: %v", err)
	}
	if _, err = f.store.GetCredentialByUserAndAuth(ctx, adminUserID, enum.Password); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the password of user-admin came back: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, PasswordFile))
	if err != nil {
		t.Error(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a restart rewrote the password file")
	}
}

// The password from the environment is the one that logs in, and it is not written down.
func TestBootstrapUsesEnvPassword(t *testing.T) {
	f, dir := newFixture(), t.TempDir()
	const password = "operator-chosen-password"
	if err := f.run(Options{DataDir: dir, AdminPassword: password}); err != nil {
		t.Fatal(err)
	}
	login(t, f.store, password)
	if got := names(t, dir); len(got) != 0 {
		t.Fatalf("data directory holds %q, want nothing", got)
	}
}

// The log names where a password is, never what it is: the file of a generated
// password on a persistent node, and the variable of one from the environment.
func TestBootstrapNeverLogsPersistentPassword(t *testing.T) {
	for _, tc := range []struct {
		name       string
		persistent bool
		env        string
	}{
		{"generated", true, ""},
		{"from the environment", true, "operator-chosen-password"},
		{"from the environment, ephemeral", false, "operator-chosen-password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := newFixture(), t.TempDir()
			opts := Options{AdminPassword: tc.env}
			if tc.persistent {
				opts.DataDir = dir
			}
			for i := 0; i < 2; i++ {
				if err := f.run(opts); err != nil {
					t.Fatal(err)
				}
			}
			// The log has to say where the password is, and not what it is.
			password, where := tc.env, AdminPasswordEnv
			if tc.env == "" {
				password, where = filePassword(t, dir), filepath.Join(dir, PasswordFile)
			}
			if strings.Contains(f.logs.String(), password) {
				t.Fatalf("the log contains the password %q:\n%s", password, f.logs)
			}
			if !strings.Contains(f.logs.String(), where) {
				t.Fatalf("the log never mentions %q:\n%s", where, f.logs)
			}
		})
	}
}

// Without a data directory the generated password has no other place to go:
// it is logged once, at WARN, with the ephemeral mode prefix.
func TestBootstrapEphemeralLogsPasswordOnce(t *testing.T) {
	f := newFixture()
	for i := 0; i < 2; i++ {
		if err := f.run(Options{}); err != nil {
			t.Fatal(err)
		}
	}
	var logged []entry
	for _, e := range f.entries(t) {
		if e.Password != "" {
			logged = append(logged, e)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("%d entries carry a password, want 1:\n%s", len(logged), f.logs)
	}
	e := logged[0]
	if e.Level != "warn" || !strings.HasPrefix(e.Message, "ephemeral mode:") {
		t.Fatalf("entry %+v, want a WARN that starts with %q", e, "ephemeral mode:")
	}
	if n := strings.Count(f.logs.String(), e.Password); n != 1 {
		t.Fatalf("the password appears %d times in the log, want once", n)
	}
	login(t, f.store, e.Password)
}

// A password from the environment must pass the password policy. The start
// fails before it seeds anything, and neither the error nor the log has the value.
func TestBootstrapRejectsEnvPasswordBreakingThePolicy(t *testing.T) {
	for _, tc := range []struct{ name, password string }{
		{"too short", "Sh0rt-pw-11"},
		{"too long", strings.Repeat("long-password-", 6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := newFixture(), t.TempDir()
			err := f.run(Options{DataDir: dir, AdminPassword: tc.password})
			if !errors.Is(err, auth.ErrWeakPassword) {
				t.Fatalf("got %v, want ErrWeakPassword", err)
			}
			if !strings.Contains(err.Error(), AdminPasswordEnv) {
				t.Fatalf("error %q does not name %s", err, AdminPasswordEnv)
			}
			if strings.Contains(err.Error(), tc.password) || strings.Contains(f.logs.String(), tc.password) {
				t.Fatal("the rejected password was echoed")
			}
			if _, err = f.store.GetUser(context.Background(), adminUserID); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("a rejected password still seeded user-admin: %v", err)
			}
			if got := names(t, dir); len(got) != 0 {
				t.Fatalf("data directory holds %q, want nothing", got)
			}
		})
	}
}

// Once users exist the variable has no effect: one WARN says so, without the
// value, and the stored password stays. That holds for a value that would
// have been refused, too.
func TestBootstrapIgnoresEnvPasswordWhenUsersExist(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		warnings       int
	}{
		{"valid", "operator-chosen-password", 1},
		{"breaking the policy", "short", 1},
		{"unset", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := newFixture(), t.TempDir()
			if err := f.run(Options{DataDir: dir}); err != nil {
				t.Fatal(err)
			}
			generated := filePassword(t, dir)
			f.logs.Reset()

			if err := f.run(Options{DataDir: dir, AdminPassword: tc.password}); err != nil {
				t.Fatal(err)
			}
			if got := f.ignoredWarnings(t); got != tc.warnings {
				t.Fatalf("%d warnings about %s, want %d:\n%s", got, AdminPasswordEnv, tc.warnings, f.logs)
			}
			if tc.password != "" && strings.Contains(f.logs.String(), tc.password) {
				t.Fatal("the log contains the ignored password")
			}
			if !passwordWorks(t, f.store, generated) {
				t.Fatal("the stored password changed")
			}
			if tc.password != "" && passwordWorks(t, f.store, tc.password) {
				t.Fatal("the ignored password was set")
			}
		})
	}
}

// Built-in roles follow the code: a start resets a role that differs (keeping
// its creation time), creates a missing one, and leaves matching and custom
// roles alone.
func TestBuiltinRolesReconciled(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	stored := func(id, name string, permissions ...enum.Permission) {
		t.Helper()
		r, err := model.NewRole(id, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range permissions {
			if err = r.PermissionAdd(p); err != nil {
				t.Fatal(err)
			}
		}
		r.SetCreatedAt(created)
		r.SetUpdatedAt(created)
		if err = f.roles.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	stored(enum.RoleAdminID, "Root", enum.UsersGet, "legacy:permission")
	stored(enum.RoleReaderID, "Reader", enum.AgentsGet, enum.UsersGet, enum.SpecsGet, enum.SpecsDelete)
	stored(enum.RoleSpecReaderID, "specReader", enum.SpecsGet)
	stored("custom", "Custom", enum.AgentsEdit)

	if err := f.run(Options{}); err != nil {
		t.Fatal(err)
	}
	for _, br := range enum.BuiltinRoles {
		got, err := f.roles.Get(ctx, br.ID)
		if err != nil {
			t.Fatalf("role %s: %v", br.ID, err)
		}
		have, want := slices.Sorted(slices.Values(got.PermissionsAll())), slices.Sorted(slices.Values(br.Permissions))
		if got.Name() != br.Name || !slices.Equal(have, want) {
			t.Errorf("role %s is %q %v, want %q %v", br.ID, got.Name(), have, br.Name, want)
		}
	}

	for _, tc := range []struct {
		id, why   string
		rewritten bool
	}{
		{enum.RoleAdminID, "differs", true},
		{enum.RoleReaderID, "differs", true},
		{enum.RoleSpecReaderID, "matches", false},
		{"custom", "is not built in", false},
	} {
		r, err := f.roles.Get(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if !r.CreatedAt().Equal(created) {
			t.Errorf("role %s lost its creation time", tc.id)
		}
		if rewritten := !r.UpdatedAt().Equal(created); rewritten != tc.rewritten {
			t.Errorf("role %s %s: rewritten = %v, want %v", tc.id, tc.why, rewritten, tc.rewritten)
		}
	}
	custom, err := f.roles.Get(ctx, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if custom.Name() != "Custom" || !slices.Equal(custom.PermissionsAll(), []enum.Permission{enum.AgentsEdit}) {
		t.Fatalf("the custom role changed to %q %v", custom.Name(), custom.PermissionsAll())
	}
}

// failingTx fails every transaction, as a store does that stops working
// between two writes of the same start.
type failingTx struct{ storage.Storage }

var errStore = errors.New("store failure")

func (failingTx) WithTx(context.Context, func(storage.Storage) error) error { return errStore }

// A first start that stops after it created user-admin, before the password is
// stored, must not leave the next start without an administrator to log in as:
// the failed start takes back its file, and the next one finishes the seed.
func TestBootstrapFinishesAFirstStartThatStoppedHalfway(t *testing.T) {
	ctx := context.Background()
	f, dir := newFixture(), t.TempDir()
	broken := credential.New(failingTx{f.store}, f.logger)
	err := Run(ctx, f.logger, Options{DataDir: dir, cost: bcrypt.MinCost}, f.roles, f.users, broken)
	if !errors.Is(err, errStore) {
		t.Fatalf("first start: %v, want the store failure", err)
	}
	created, err := f.store.GetUser(ctx, adminUserID)
	if err != nil {
		t.Fatalf("user-admin was not created before the failure: %v", err)
	}
	if _, err = f.store.GetCredentialByUserAndAuth(ctx, adminUserID, enum.Password); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a password was stored: %v", err)
	}
	if got := names(t, dir); len(got) != 0 {
		t.Fatalf("the failed start left %q, a password that was never stored", got)
	}

	if err = f.run(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	login(t, f.store, filePassword(t, dir))
	finished, err := f.store.GetUser(ctx, adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !finished.CreatedAt().Equal(created.CreatedAt()) {
		t.Fatal("the second start created user-admin again")
	}
}

// The password is written down before it is stored, so a start cannot end with
// a stored password that nobody can read. A failed write leaves no file behind.
func TestBootstrapStoresNoPasswordItCannotWriteDown(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
		left []string // what the data directory still holds
	}{
		{"directory is missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, nil},
		{"name is taken by a directory", func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, PasswordFile), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, []string{PasswordFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := newFixture(), tc.dir(t)
			if err := f.run(Options{DataDir: dir}); err == nil {
				t.Fatal("bootstrap succeeded without a place for the password")
			}
			if _, err := f.store.GetUser(context.Background(), adminUserID); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("user-admin was created: %v", err)
			}
			if tc.left != nil {
				if got := names(t, dir); !slices.Equal(got, tc.left) {
					t.Fatalf("data directory holds %q, want %q", got, tc.left)
				}
			}
		})
	}
}
