package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/metrics"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// ObserveTurns hears each agent's turn once, with how it ended: answered when the
// agent answered a call, unanswered when a call got no answer, and local when the
// turn needed no call.
func TestObserveTurnsReportsEachTurnOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		fp   *fakeProxy
		seed func(t *testing.T, r *Runner, store storage.Storage)
		want string
	}{
		{
			name: "answered",
			fp:   &fakeProxy{applyResp: []string{"task-1"}},
			seed: func(t *testing.T, _ *Runner, store storage.Storage) {
				ro, _ := model.NewRollout("sp-1", "agent-a", 1)
				ro.SetIntent(enum.RolloutIntentInstall)
				if err := store.UpsertRollout(context.Background(), ro); err != nil {
					t.Fatal(err)
				}
			},
			want: metrics.TurnAnswered,
		},
		{
			name: "unanswered",
			fp:   &fakeProxy{applyErr: []error{proxy.ErrRequest}},
			seed: func(t *testing.T, _ *Runner, store storage.Storage) {
				ro, _ := model.NewRollout("sp-1", "agent-a", 1)
				ro.SetIntent(enum.RolloutIntentInstall)
				if err := store.UpsertRollout(context.Background(), ro); err != nil {
					t.Fatal(err)
				}
			},
			want: metrics.TurnUnanswered,
		},
		{
			name: "local",
			fp:   &fakeProxy{},
			seed: func(t *testing.T, _ *Runner, store storage.Storage) {
				ro, _ := model.NewRollout("sp-1", "agent-a", 1)
				ro.SetIntent(enum.RolloutIntentUninstall) // no task ID: dropped without a call
				ro.MarkPending(1)
				if err := store.UpsertRollout(context.Background(), ro); err != nil {
					t.Fatal(err)
				}
			},
			want: metrics.TurnLocal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _ := newFakeRunner(t, tc.fp)
			r.backoffs = map[string]agentBackoff{}
			r.incomplete = map[string]string{}
			_ = seedSpecAndAgent(t, store, "sp-1", "agent-a")
			tc.seed(t, r, store)
			var got []string
			r.ObserveTurns(func(outcome string, d time.Duration) {
				if d < 0 {
					t.Errorf("duration %v", d)
				}
				got = append(got, outcome)
			})
			r.reconcileRollouts(context.Background())
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("turn outcomes %v, want [%s]", got, tc.want)
			}
		})
	}
}

// A tick that is cancelled reports no turn: it says nothing about the agent.
func TestObserveTurnsSkipsCancelledTicks(t *testing.T) {
	r, store, _ := newFakeRunner(t, &fakeProxy{applyErr: []error{errors.New("x")}})
	r.backoffs = map[string]agentBackoff{}
	r.incomplete = map[string]string{}
	_ = seedSpecAndAgent(t, store, "sp-1", "agent-a")
	ro, _ := model.NewRollout("sp-1", "agent-a", 1)
	ro.SetIntent(enum.RolloutIntentInstall)
	if err := store.UpsertRollout(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	reported := 0
	r.ObserveTurns(func(string, time.Duration) { reported++ })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.reconcileRollouts(ctx)
	if reported != 0 {
		t.Fatalf("a cancelled tick reported %d turns", reported)
	}
}
