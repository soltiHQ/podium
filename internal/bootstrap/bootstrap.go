// Package bootstrap seeds initial data required for the control-plane.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/role"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const (
	adminUserID  = "user-admin"
	adminSubject = "admin"

	// AdminPasswordEnv is the environment variable that carries the initial administrator password.
	AdminPasswordEnv = "SOLTI_BOOTSTRAP_ADMIN_PASSWORD"
	// PasswordFile is the file in [Options.DataDir] that receives a generated administrator password.
	PasswordFile = "bootstrap-admin-password"
)

// ErrNoDataDir is returned when the administrator has to be seeded and [Options]
// names neither a data directory nor ephemeral state: a generated password would
// have nowhere to go, and bootstrap never falls back to the log.
var ErrNoDataDir = errors.New("bootstrap: no data directory and the state is not ephemeral")

// Options says where a generated administrator password goes and which password to use instead.
type Options struct {
	// DataDir receives the file with a generated administrator password.
	DataDir string
	// Ephemeral says the state lives in memory only, so there is no data directory:
	// a generated password is logged, once, instead of written down. Nothing else
	// ever logs a password. With neither DataDir nor Ephemeral a seed fails with
	// [ErrNoDataDir]; when both are set, DataDir wins.
	Ephemeral bool
	// AdminPassword is the initial administrator password, from [AdminPasswordEnv].
	// Empty means bootstrap generates one.
	AdminPassword string

	// cost is the bcrypt cost of the password; zero is the default. Tests lower it.
	cost int
}

// Run reconciles the built-in roles with their code definitions and seeds the
// administrator. User-admin and its password are seeded when the user table is
// empty, its password alone when user-admin is the only user and has none (a
// first start that stopped halfway). Any other user table is left alone, so a
// deleted user-admin stays deleted.
func Run(ctx context.Context, logger zerolog.Logger, opts Options, roleSVC *role.Service, userSVC *user.Service, credSVC *credential.Service) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	logger.Debug().Msg("bootstrap: starting")
	if err := reconcileRoles(ctx, logger, roleSVC); err != nil {
		return err
	}
	return seedAdmin(ctx, logger, opts, userSVC, credSVC)
}

// reconcileRoles creates the built-in roles that are missing and resets the name
// and permissions of those that differ from enum.BuiltinRoles, keeping their
// creation time. A role that matches is not written. Nothing edits roles at
// runtime, so a difference comes from an upgrade of the definitions.
func reconcileRoles(ctx context.Context, logger zerolog.Logger, roleSVC *role.Service) error {
	for _, br := range enum.BuiltinRoles {
		want, err := model.NewRole(br.ID, br.Name)
		if err != nil {
			return err
		}
		for _, p := range br.Permissions {
			if err = want.PermissionAdd(p); err != nil {
				return err
			}
		}

		have, err := roleSVC.Get(ctx, br.ID)
		switch {
		case err == nil:
			if sameRole(have, want) {
				continue
			}
			want.SetCreatedAt(have.CreatedAt())
		case !errors.Is(err, storage.ErrNotFound):
			return err
		}
		if err = roleSVC.Upsert(ctx, want); err != nil {
			return err
		}
		logger.Debug().Str("role_id", br.ID).Str("name", br.Name).Msg("bootstrap: role reconciled")
	}
	return nil
}

// sameRole reports whether a and b have the same name and the same set of permissions.
func sameRole(a, b *model.Role) bool {
	if a.Name() != b.Name() {
		return false
	}
	ap, bp := a.PermissionsAll(), b.PermissionsAll()
	slices.Sort(ap)
	slices.Sort(bp)
	return slices.Equal(ap, bp)
}

// adminSeed says what seeding the administrator takes.
type adminSeed int

const (
	// seedNone: users exist, leave them alone.
	seedNone adminSeed = iota
	// seedUser: the user table is empty, create user-admin and its password.
	seedUser
	// seedPassword: user-admin is the only user and has no password, a first start
	// stopped after creating it.
	seedPassword
)

func seedAdmin(ctx context.Context, logger zerolog.Logger, opts Options, userSVC *user.Service, credSVC *credential.Service) error {
	seed, err := adminSeedNeeded(ctx, userSVC, credSVC)
	if err != nil {
		return err
	}
	if seed == seedNone {
		if opts.AdminPassword != "" {
			logger.Warn().Msg("bootstrap: " + AdminPasswordEnv + " is ignored: users already exist, remove it from the environment")
		}
		return nil
	}

	// Fail closed: without a data directory only a node that says its state is
	// ephemeral may go on, and never by guessing from an empty path.
	if opts.DataDir == "" && !opts.Ephemeral {
		return ErrNoDataDir
	}

	password, generated, err := initialPassword(opts)
	if err != nil {
		return err
	}
	// The generated password is written down before it is stored. A start that
	// stops in between leaves a file nobody needs, never a stored password
	// nobody can read. A start that fails while storing keeps its file too: the
	// write may have taken effect (raft can report a lost leadership after the
	// entry committed, and a failed fsync leaves the outcome open), and then the
	// file is the only readable copy. The next seed replaces it whole.
	var path string
	if generated && opts.DataDir != "" {
		if path, err = writePasswordFile(opts.DataDir, password); err != nil {
			return fmt.Errorf("bootstrap: write admin password file: %w", err)
		}
		path = absPath(path)
	}
	if err = storeAdmin(ctx, logger, opts, seed, password, userSVC, credSVC); err != nil {
		if path != "" {
			return fmt.Errorf("bootstrap: store the admin password written to %s: %w", path, err)
		}
		return err
	}

	// The password is logged only on the ephemeral branch; a state that fits none
	// of the branches logs nothing.
	switch {
	case !generated:
		logger.Info().Str("login", adminSubject).Msg("bootstrap: admin password set from " + AdminPasswordEnv)
		removeStalePasswordFile(logger, opts.DataDir)
	case path != "":
		logger.Warn().Str("login", adminSubject).Str("path", path).Msg("bootstrap: admin password generated: read it from the file, log in and change it")
	case opts.Ephemeral:
		logger.Warn().Str("login", adminSubject).Str("password", password).Msg("ephemeral mode: admin password generated: change it after first login")
	}
	return nil
}

// removeStalePasswordFile removes the password file an earlier generated seed
// left in dir: the password just stored does not match it. The password is
// stored by now, so a file that cannot be removed only warns.
func removeStalePasswordFile(logger zerolog.Logger, dir string) {
	if dir == "" {
		return
	}
	path := absPath(filepath.Join(dir, PasswordFile))
	switch err := os.Remove(path); {
	case err == nil:
		logger.Info().Str("path", path).Msg("bootstrap: stale admin password file removed: the password comes from " + AdminPasswordEnv)
	case !errors.Is(err, os.ErrNotExist):
		logger.Warn().Err(err).Str("path", path).Msg("bootstrap: remove stale admin password file")
	}
}

// absPath returns path made absolute for the log, or path itself when the
// working directory is unknown.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// adminSeedNeeded decides what seeding the administrator takes. Restart must
// preserve operator-managed identities and credentials, so a seed is due only on
// an empty user table, or when user-admin is the only user and has no password:
// that is all a first start that stopped halfway leaves behind. An administrator
// the operator deleted does not come back, and an established system (any other
// user) is never given a new password, however its user-admin looks.
func adminSeedNeeded(ctx context.Context, userSVC *user.Service, credSVC *credential.Service) (adminSeed, error) {
	page, err := userSVC.List(ctx, user.ListQuery{Limit: 2})
	if err != nil {
		return seedNone, err
	}
	switch {
	case len(page.Items) == 0:
		return seedUser, nil
	case len(page.Items) > 1 || page.Items[0] == nil || page.Items[0].ID() != adminUserID:
		return seedNone, nil
	}

	creds, err := credSVC.ListByUser(ctx, credential.ListByUserQuery{UserID: adminUserID})
	if err != nil {
		return seedNone, err
	}
	for _, cred := range creds.Items {
		if cred.AuthKind() == enum.Password {
			return seedNone, nil
		}
	}
	return seedPassword, nil
}

// initialPassword returns the password of the new administrator and whether
// bootstrap generated it. One from the environment must pass the password
// policy; its error names the variable and never the value.
func initialPassword(opts Options) (password string, generated bool, err error) {
	if opts.AdminPassword != "" {
		if err = credentials.CheckPasswordPolicy(opts.AdminPassword); err != nil {
			return "", false, fmt.Errorf("bootstrap: %s must have at least %d characters and at most %d bytes: %w",
				AdminPasswordEnv, credentials.MinPasswordLength, credentials.MaxPasswordBytes, err)
		}
		return opts.AdminPassword, false, nil
	}
	if password, err = credentials.GeneratePassword(0); err != nil {
		return "", false, fmt.Errorf("bootstrap: generate admin password: %w", err)
	}
	return password, true, nil
}

// storeAdmin creates user-admin when seed asks for it, then sets its password
// through the credential service, which applies the password policy.
func storeAdmin(ctx context.Context, logger zerolog.Logger, opts Options, seed adminSeed, password string, userSVC *user.Service, credSVC *credential.Service) error {
	if seed == seedUser {
		u, err := model.NewUser(adminUserID, adminSubject)
		if err != nil {
			return err
		}
		u.SetEmail("admin@solti.local")
		u.SetName("admin")
		if err = u.RoleAdd(enum.RoleAdminID); err != nil {
			return err
		}
		if err = userSVC.Upsert(ctx, u); err != nil {
			return err
		}
		logger.Debug().Str("user_id", adminUserID).Msg("bootstrap: admin user seeded")
	}
	return credSVC.SetPassword(ctx, credential.SetPasswordRequest{
		UserID:   adminUserID,
		Password: password,
		Cost:     opts.cost,
	})
}

// writePasswordFile stores password in dir/PasswordFile with mode 0600 and
// returns the path. The password goes to a temporary file in dir, which is
// synced and renamed over the target, so the target is never half written and
// a stale file is replaced whole.
func writePasswordFile(dir, password string) (string, error) {
	path := filepath.Join(dir, PasswordFile)
	f, err := os.CreateTemp(dir, PasswordFile+".*.tmp") // mode 0600
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(password + "\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	// The rename reaches the disk only with its directory.
	if err = syncDir(dir); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}
