package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	agentsvc "github.com/soltiHQ/control-plane/internal/service/agent"
)

// The secrets and the token the tests below derive by hand (openssl dgst
// -sha256 -hmac): HMAC-SHA256 of "agent-a", a NUL byte and "1", under
// tokenSecret, as lowercase hex.
const (
	tokenSecret      = "0123456789abcdef0123456789abcdef"
	otherTokenSecret = "a-different-secret-of-32-bytes-or-more!"
	agentAToken1     = "0798a24179a424ee55aad934c6bc3ddc684517034d842088a2629219ad29fbcf"
)

// Podium stores no token for a derived credential: it recomputes the token from
// the enrollment secret, the agent ID and the generation, and presents that on
// its calls to the agent. An agent whose credential derives from another secret
// gets no call at all: the push fails with the text of the refusal.
func TestOutboundUsesDerivedToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret string
		want   string // the Authorization the agent receives, "" for no call
		failed string // the start of the rollout error when there is no call
	}{
		{"derived under the current secret", tokenSecret, "Bearer " + agentAToken1, ""},
		{"derived under another secret", otherTokenSecret, "", "agent credential lookup: agent agent-a enrolled under another enrollment secret: it needs the token that podium agent-token --generation 1 agent-a prints with the current secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			got := make(chan string, 8)
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get("Authorization")
				http.Error(w, "refused by the test agent", http.StatusServiceUnavailable)
			}))
			t.Cleanup(agent.Close)

			r, s, _ := newFakeRunner(t, &fakeProxy{})
			pool := proxy.NewPool(nil, true)
			t.Cleanup(func() { _ = pool.Close() })
			r.pool = pool
			r.tokens = agentsvc.New(s, zerolog.Nop(), agentsvc.WithEnrollSecret(tc.secret))
			pushed := seedRollout(t, s, "sp-pushed", "agent-a", enum.RolloutIntentInstall, "")
			ag, err := model.NewAgentFrom(model.AgentParams{ID: "agent-a", Name: "agent-a", Endpoint: agent.URL, EndpointType: 2, APIVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpsertAgent(ctx, ag); err != nil {
				t.Fatal(err)
			}
			cred, err := model.NewDerivedAgentCredential("agent-a", 1, agentsvc.SecretID(tokenSecret))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpsertAgentCredential(ctx, cred); err != nil {
				t.Fatal(err)
			}

			r.tick(ctx)

			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("the agent of another secret got %d calls, want none", len(got))
				}
				if after := mustRollout(t, s, pushed.ID()); after.Status() != enum.SyncStatusFailed || !strings.HasPrefix(after.Error(), tc.failed) {
					t.Fatalf("the push is %s: %q, want it failed with %q", after.Status(), after.Error(), tc.failed)
				}
				return
			}
			select {
			case auth := <-got:
				if auth != tc.want {
					t.Fatalf("the agent received authorization %q, want %q", auth, tc.want)
				}
			default:
				t.Fatal("the agent got no call")
			}
		})
	}
}
