package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	historysvc "github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/storage"
)

type archiveProxy struct {
	fakeProxy
	fail    error
	running bool
	cancels int
	order   []string
}

func (f *archiveProxy) GetTask(context.Context, string) (*proxyv1.GetTaskResponse, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return &proxyv1.GetTaskResponse{Task: &proxyv1.Task{ID: "task", UID: "uid"}}, nil
}

func (f *archiveProxy) ListTaskRunsPage(context.Context, string, proxy.TaskRunQuery) (*proxy.TaskRunPage, error) {
	f.order = append(f.order, "capture")
	status := "failed"
	finished := true
	if f.running {
		status = "running"
		finished = false
	}
	return &proxy.TaskRunPage{TaskUID: "uid", ResourceVersion: "rv", Runs: []proxyv1.TaskRun{{Generation: 1, Attempt: 1, Status: status, FinishedAtPresent: finished, FinishedAt: 50, StartedAt: 100}}}, nil
}
func (f *archiveProxy) CancelTask(context.Context, string) error { f.cancels++; return nil }
func (f *archiveProxy) DeleteTask(ctx context.Context, id string) error {
	f.order = append(f.order, "delete")
	return f.fakeProxy.DeleteTask(ctx, id)
}

type archivePool struct{ source proxy.AgentProxy }

func (p archivePool) Get(string, enum.EndpointType, enum.APIVersion, string) (proxy.AgentProxy, error) {
	return p.source, nil
}

func TestUninstallCapturesObservedTerminalBeforeNormalDeletion(t *testing.T) {
	for _, name := range []string{"terminal", "running", "source-unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := &archiveProxy{}
			if name == "running" {
				f.running = true
			}
			if name == "source-unavailable" {
				f.fail = errors.New("offline")
			}
			r, s, _ := newFakeRunner(t, &f.fakeProxy)
			r.pool = archivePool{f}
			svc, err := historysvc.New(s, historysvc.Config{})
			if err != nil {
				t.Fatal(err)
			}
			r.history = svc
			seedSpecAndAgent(t, s, "spec", "agent")
			rollout, err := model.NewRollout("spec", "agent", 1)
			if err != nil {
				t.Fatal(err)
			}
			rollout.SetActualTaskID("task")
			rollout.SetIntent(enum.RolloutIntentUninstall)
			if err := s.UpsertRollout(context.Background(), rollout); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r.reconcileUninstall(ctx, rollout)
			if len(f.deletes) != 1 || f.cancels != 0 {
				t.Fatalf("archive changed deletion/cancellation behavior deletes=%v cancels=%d", f.deletes, f.cancels)
			}
			if _, err := s.GetRollout(context.Background(), rollout.ID()); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("normal uninstall did not complete")
			}
			rows, err := storage.AllExecutions(context.Background(), s, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := 0
			if name == "terminal" {
				expected = 1
				if len(f.order) != 2 || f.order[0] != "capture" || f.order[1] != "delete" {
					t.Fatalf("wrong lifecycle order %v", f.order)
				}
			}
			if len(rows) != expected {
				t.Fatalf("unexpected synthetic archive result %d", len(rows))
			}
			if expected == 1 && (rows[0].Status() != "failed" || rows[0].TaskUID() != "uid" || rows[0].SpecID() != "spec") {
				t.Fatal("archive lost terminal identity")
			}
		})
	}
}
