package credential

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Every way of setting a password applies the policy, the trusted internal
// one included, and a rejected password stores nothing.
func TestSetPasswordRejectsWeakPasswords(t *testing.T) {
	ctx := context.Background()
	s := inmemory.New()
	u, err := model.NewUser("user", "subject")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	svc := New(s, zerolog.Nop())
	self := &identity.Identity{UserID: u.ID()}
	for _, password := range []string{strings.Repeat("a", 11), strings.Repeat("a", 73)} {
		req := SetPasswordRequest{UserID: u.ID(), Password: password, Cost: 4}
		if err := svc.SetPassword(ctx, req); !errors.Is(err, auth.ErrWeakPassword) {
			t.Errorf("SetPassword with %d bytes: %v, want ErrWeakPassword", len(password), err)
		}
		if err := svc.SetPasswordAs(ctx, self, req); !errors.Is(err, auth.ErrWeakPassword) {
			t.Errorf("SetPasswordAs with %d bytes: %v, want ErrWeakPassword", len(password), err)
		}
	}
	if _, err := s.GetCredentialByUserAndAuth(ctx, u.ID(), enum.Password); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a rejected password was stored: %v", err)
	}
}
