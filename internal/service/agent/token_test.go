package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
)

// Enrollment refuses a token over 1024 bytes and stores nothing. The cap is an
// enrollment rule only: a credential stored with a longer token, by an older
// version, still verifies.
func TestVerifyOrEnrollTokenCapsOnlyNewTokens(t *testing.T) {
	ctx := context.Background()
	store := inmemory.New()
	svc := New(store, zerolog.New(io.Discard))

	if err := svc.VerifyOrEnrollToken(ctx, "agent-a", strings.Repeat("t", 1025)); !errors.Is(err, ErrTokenTooLong) {
		t.Fatalf("enroll a 1025-byte token: %v, want ErrTokenTooLong", err)
	}
	if token, err := svc.AgentToken(ctx, "agent-a"); err != nil || token != "" {
		t.Fatalf("refused enrollment stored a token: %d bytes, %v", len(token), err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-a", strings.Repeat("t", 1024)); err != nil {
		t.Fatalf("enroll a 1024-byte token: %v", err)
	}

	stored := strings.Repeat("s", 4096)
	legacy, err := model.NewAgentCredential("agent-b", stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgentCredential(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-b", stored); err != nil {
		t.Fatalf("verify a stored 4096-byte token: %v", err)
	}
	if err := svc.VerifyOrEnrollToken(ctx, "agent-b", "other-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong token for agent-b: %v, want ErrUnauthenticated", err)
	}
}
