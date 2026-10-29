package bootstrap

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
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
	if err := seedRoles(ctx, logger, roles); err != nil {
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
		if err := Run(ctx, logger, roles, users, creds); err != nil {
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
	if err := seedRoles(ctx, logger, roles); err != nil {
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
	if err := Run(ctx, logger, roles, users, creds); err != nil {
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
	if err := Run(ctx, logger, roles, users, creds); err != nil {
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
	err := Run(ctx, logger, role.New(store, logger), user.New(store, logger), credential.New(store, logger))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := store.GetUser(context.Background(), adminUserID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unexpected user: %v", err)
	}
}
