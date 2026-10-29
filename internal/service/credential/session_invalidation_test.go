package credential

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

func TestPasswordResetInvalidatesAll503Sessions(t *testing.T) {
	s := inmemory.New()
	ctx := context.Background()
	u, _ := model.NewUser("user", "subject")
	_ = s.UpsertUser(ctx, u)
	for i := 0; i < 503; i++ {
		sess, _ := model.NewSession(fmt.Sprintf("s-%d", i), u.ID(), "cred-user", enum.Password, []byte("hash"), time.Now().Add(time.Hour))
		_ = s.CreateSession(ctx, sess)
	}
	svc := New(s, zerolog.New(io.Discard))
	if err := svc.SetPassword(ctx, SetPasswordRequest{UserID: u.ID(), Password: "new password", Cost: 4}); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ListSessionsByUser(ctx, u.ID())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions survive reset: %d %v", len(sessions), err)
	}
}
