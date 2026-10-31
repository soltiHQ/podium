package sync

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/event"
)

// A change to the tasks an agent holds reaches the pages that show them: a push
// applied, an uninstall done and a lost task queued for a reinstall send
// agent_update, as they send spec_update. A push that failed changes nothing on
// the agent, and the uninstall of a task the agent never got makes no call: they
// send spec_update only.
func TestTaskChangesNotifyTheAgentPages(t *testing.T) {
	both := []string{event.RefreshAgents, event.RefreshSpecs}
	specsOnly := []string{event.RefreshSpecs}
	for _, tc := range []struct {
		name string
		fp   *fakeProxy
		seed func(t *testing.T, r *Runner)
		want []string // the kinds sent, in name order
	}{
		{
			name: "push applied",
			fp:   &fakeProxy{applyResp: []string{"task-new"}},
			seed: func(t *testing.T, r *Runner) {
				_ = seedRollout(t, r.store, "sp-1", "agent-a", enum.RolloutIntentInstall, "")
			},
			want: both,
		},
		{
			name: "uninstall done",
			fp:   &fakeProxy{tasks: map[string]struct{}{"task-old": {}}},
			seed: func(t *testing.T, r *Runner) {
				_ = seedRollout(t, r.store, "sp-1", "agent-a", enum.RolloutIntentUninstall, "task-old")
			},
			want: both,
		},
		{
			name: "lost task queued for a reinstall",
			fp:   &fakeProxy{},
			seed: func(t *testing.T, r *Runner) {
				_ = seedRollout(t, r.store, "sp-1", "agent-a", enum.RolloutIntentNoop, "task-lost")
			},
			want: both,
		},
		{
			name: "push failed",
			fp:   &fakeProxy{applyResp: []string{""}, applyErr: []error{errors.New("agent down")}},
			seed: func(t *testing.T, r *Runner) {
				_ = seedRollout(t, r.store, "sp-1", "agent-a", enum.RolloutIntentInstall, "")
			},
			want: specsOnly,
		},
		{
			name: "uninstall of a task the agent never got",
			fp:   &fakeProxy{},
			seed: func(t *testing.T, r *Runner) {
				_ = seedRollout(t, r.store, "sp-1", "agent-a", enum.RolloutIntentUninstall, "")
			},
			want: specsOnly,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, hub := newFakeRunner(t, tc.fp)
			tc.seed(t, r)
			pulses := hub.Subscribe(t.Context())

			r.tick(context.Background())

			var got []string
			for len(pulses) > 0 {
				got = append(got, <-pulses)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the tick sent %v, want %v", got, tc.want)
			}
		})
	}
}
