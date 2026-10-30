package sync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
)

// Lifecycle deletes only an agent's row: its rollouts and its credential stay.
// A sync over those rollouts records "agent not found" on each push, until
// max_retries, makes no call for an observation, and leaves the agent's backoff
// as it was: no call went out, so there is nothing to learn about the agent.
func TestSyncOverRolloutsOfADeletedAgent(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		before *agentBackoff // nil: none
	}{
		{"no backoff", nil},
		{"backoff passed", &agentBackoff{failures: 2, until: time.Now().Add(-time.Second), status: enum.AgentStatusActive, uptime: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{}
			r, s, _ := newFakeRunner(t, fp)
			pending := seedRollout(t, s, "sp-pending", "agent-a", enum.RolloutIntentInstall, "")
			synced := seedRollout(t, s, "sp-synced", "agent-a", enum.RolloutIntentNoop, "task-synced")
			cred, err := model.NewAgentCredential("agent-a", "stored-token")
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpsertAgentCredential(ctx, cred); err != nil {
				t.Fatal(err)
			}
			if tc.before != nil {
				r.backoffs = map[string]agentBackoff{"agent-a": *tc.before}
			}
			if err = s.DeleteAgent(ctx, "agent-a"); err != nil { // what lifecycle deletion does
				t.Fatal(err)
			}

			r.tick(ctx)

			if calls := len(fp.applies) + len(fp.deletes) + len(fp.gets); calls != 0 {
				t.Fatalf("the deleted agent got %d calls, want none", calls)
			}
			after, err := s.GetRollout(ctx, pending.ID())
			if err != nil {
				t.Fatal(err)
			}
			if after.Status() != enum.SyncStatusFailed || after.Attempts() != 1 || !strings.HasPrefix(after.Error(), "agent not found: ") {
				t.Fatalf("push to the deleted agent: status=%s attempts=%d error=%q, want one failure, agent not found", after.Status(), after.Attempts(), after.Error())
			}
			kept, err := s.GetRollout(ctx, synced.ID())
			if err != nil {
				t.Fatal(err)
			}
			if kept.Status() != enum.SyncStatusSynced || !kept.UpdatedAt().Equal(synced.UpdatedAt()) {
				t.Fatalf("observation of the deleted agent changed its rollout: status=%s", kept.Status())
			}
			b, ok := r.backoffs["agent-a"]
			switch {
			case tc.before == nil && ok:
				t.Fatalf("the deleted agent got a backoff: %+v", b)
			case tc.before != nil && (!ok || b.failures != tc.before.failures || !b.until.Equal(tc.before.until) ||
				b.status != tc.before.status || b.uptime != tc.before.uptime):
				t.Fatalf("the deleted agent's backoff is %+v (kept %v), want %+v", b, ok, *tc.before)
			}
		})
	}
}
