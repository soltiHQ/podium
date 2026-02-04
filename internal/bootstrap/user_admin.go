package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"

	"github.com/soltiHQ/control-plane/auth/credentials"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"

	"github.com/rs/zerolog"
)

const (
	AdminUserID       = "00000000-0000-0000-0000-000000000001"
	AdminSubject      = "bootstrap:admin"
	AdminCredentialID = "00000000-0000-0000-0000-000000000002"
)

type EnsureAdminUserStep struct{}

func (EnsureAdminUserStep) Name() string {
	return "ensure_admin_user"
}

func (EnsureAdminUserStep) Run(ctx context.Context, logger zerolog.Logger, store storage.Storage) error {
	if store == nil {
		return storage.ErrInvalidArgument
	}
	if _, err := store.GetRole(ctx, AdminRoleID); err != nil {
		return err
	}

	var (
		user          *domain.UserModel
		userCreated   bool
		userUpdated   bool
		credCreated   bool
		password      string
		wasGenerated  bool
		shouldLogPass bool
	)
	u, err := store.GetUserBySubject(ctx, AdminSubject)
	if err == nil {
		user = u
	} else {
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}

		password, wasGenerated = getOrGeneratePassword()
		shouldLogPass = wasGenerated

		admin, err := domain.NewUserModel(AdminUserID, AdminSubject)
		if err != nil {
			return err
		}
		if err = admin.RoleAdd(AdminRoleID); err != nil {
			return err
		}
		if err = store.UpsertUser(ctx, admin); err != nil {
			return err
		}

		user = admin
		userCreated = true
	}

	if user == nil {
		return storage.ErrInternal
	}
	userID := user.ID()

	if !user.RoleHas(AdminRoleID) {
		if err := user.RoleAdd(AdminRoleID); err != nil {
			return err
		}
		if err := store.UpsertUser(ctx, user); err != nil {
			return err
		}
		userUpdated = true
	}

	c, err := store.GetCredential(ctx, AdminCredentialID)
	if err == nil {
		if c.UserID() != userID {
			return storage.ErrConflict
		}
		if c.Type() != domain.CredentialTypePassword {
			return storage.ErrConflict
		}
	} else {
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}

		if password == "" {
			password, wasGenerated = getOrGeneratePassword()
			shouldLogPass = wasGenerated
		}

		cred, err := credentials.NewPasswordCredential(
			AdminCredentialID,
			userID,
			password,
		)
		if err != nil {
			return err
		}
		if err = store.UpsertCredential(ctx, cred); err != nil {
			return err
		}
		credCreated = true
	}

	if !userCreated && !userUpdated && !credCreated {
		return nil
	}
	ev := logger.Warn().
		Str("user_id", userID).
		Str("subject", AdminSubject).
		Str("role_id", AdminRoleID).
		Bool("user_created", userCreated).
		Bool("user_updated", userUpdated).
		Bool("credential_created", credCreated)

	if credCreated && shouldLogPass {
		ev.Str("password", password).
			Msg("BOOTSTRAP: Ensured admin user/credential with GENERATED password - SAVE IT NOW")
		return nil
	}

	ev.Msg("BOOTSTRAP: Ensured admin user/credential")
	return nil
}

func getOrGeneratePassword() (password string, wasGenerated bool) {
	if pw := os.Getenv("CP_ADMIN_PASSWORD"); pw != "" {
		return pw, false
	}
	return generateSecurePassword(32), true
}

func generateSecurePassword(length int) string {
	if length <= 0 {
		return ""
	}

	var (
		n = (length*3 + 3) / 4
		b = make([]byte, n)
	)

	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}

	s := base64.RawURLEncoding.EncodeToString(b)
	if len(s) < length {
		return s
	}
	return s[:length]
}
