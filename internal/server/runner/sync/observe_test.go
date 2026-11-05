package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

// unchanged reports whether the stored rollout is still the one that was seeded.
func unchanged(t *testing.T, s storage.Storage, seeded *model.Rollout) bool {
	t.Helper()
	after, err := s.GetRollout(context.Background(), seeded.ID())
	if err != nil {
		t.Fatal(err)
	}
	return after.UpdatedAt().Equal(seeded.UpdatedAt()) && after.Intent() == seeded.Intent() && after.ActualTaskID() == seeded.ActualTaskID()
}

// requeued reports whether the stored rollout is queued for a reinstall.
func requeued(t *testing.T, s storage.Storage, seeded *model.Rollout) bool {
	t.Helper()
	after, err := s.GetRollout(context.Background(), seeded.ID())
	if err != nil {
		t.Fatal(err)
	}
	return after.Intent() == enum.RolloutIntentInstall && after.Status() == enum.SyncStatusPending && after.ActualTaskID() == ""
}

// tooLarge is what the proxy returns for a page whose answer it refused for its
// size: the decode error that says why.
func tooLarge() error {
	return agentErr{errkind.Internal, fmt.Errorf("%w: %w: %w: agent response exceeds 4 MiB", proxy.ErrListTasks, proxy.ErrDecode, proxy.ErrResponseTooLarge)}
}

// tooManyItems is what the proxy returns for a page that holds more items than the
// limit: the decode error that says so, which is no page too large.
func tooManyItems() error {
	return agentErr{errkind.Internal, fmt.Errorf("%w: %w: %w: more than 1000 items", proxy.ErrListTasks, proxy.ErrDecode, proxy.ErrTooManyItems)}
}

// tooManyElementsErr is what the proxy returns for an answer that holds more
// repeated elements than it builds in all: a decode error, and an ErrTooManyItems
// too. A smaller page may come under it.
func tooManyElementsErr() error {
	return agentErr{errkind.Internal, fmt.Errorf("%w: %w: %w: more than 262144 elements in an answer", proxy.ErrListTasks, proxy.ErrDecode, proxy.ErrTooManyElements)}
}

// tooManyConditionsErr is what the proxy returns for a task with more conditions
// than a task may hold: a decode error, and an ErrTooManyItems too, but no
// ErrTooManyElements. No smaller page avoids the task.
func tooManyConditionsErr() error {
	return agentErr{errkind.Internal, fmt.Errorf("%w: %w: %w: a task holds more than 64", proxy.ErrListTasks, proxy.ErrDecode, proxy.ErrTooManyConditions)}
}

// cursorTooLongErr is what the proxy returns for a page whose next cursor is over
// the limit.
func cursorTooLongErr() error {
	return agentErr{errkind.Internal, fmt.Errorf("%w: %w: %w: next cursor of 4097 bytes, over 4096", proxy.ErrListTasks, proxy.ErrDecode, proxy.ErrCursorTooLong)}
}

// holdN makes the agent hold n tasks, task-0000 on.
func holdN(fp *fakeProxy, n int) {
	for i := range n {
		fp.hold(fmt.Sprintf("task-%04d", i))
	}
}

// Observing costs one list per agent per tick, however many rollouts the agent
// has, and no GetTask. The list has no filter: a task is there whatever its
// phase, as it was to GetTask.
func TestObserveListsOncePerAgent(t *testing.T) {
	big, small := &fakeProxy{}, &fakeProxy{}
	r, s, _ := newFakeRunner(t, &fakeProxy{})
	r.pool = &fakePool{agents: map[string]proxy.AgentProxy{"http://agent-a": big, "http://agent-b": small}}
	bigRollouts := seedRollouts(t, s, "agent-a", 50, enum.RolloutIntentNoop)
	smallRollouts := seedRollouts(t, s, "agent-b", 3, enum.RolloutIntentNoop)
	big.hold(taskIDs(bigRollouts)...)
	small.hold(taskIDs(smallRollouts)...)

	r.tick(context.Background())

	for _, tc := range []struct {
		name     string
		agent    *fakeProxy
		rollouts []*model.Rollout
	}{
		{"agent-a", big, bigRollouts},
		{"agent-b", small, smallRollouts},
	} {
		if len(tc.agent.lists) != 1 || len(tc.agent.gets) != 0 {
			t.Fatalf("%s got %d lists and %d gets for %d rollouts, want 1 and 0", tc.name, len(tc.agent.lists), len(tc.agent.gets), len(tc.rollouts))
		}
		if want := (proxy.TaskFilter{Limit: 1000}); tc.agent.lists[0] != want {
			t.Fatalf("%s listed with %+v, want %+v", tc.name, tc.agent.lists[0], want)
		}
		for _, ro := range tc.rollouts {
			if !unchanged(t, s, ro) {
				t.Fatalf("rollout %s changed though its agent holds its task", ro.ID())
			}
		}
	}
}

// An agent with more tasks than a page holds is listed to the end, a page at a
// time, each from the cursor of the one before. Its tasks on every page are
// there; the one it lost is not.
func TestObservePaginates(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	holdN(fp, 2300)
	var kept []*model.Rollout
	for _, task := range []string{"task-0000", "task-1500", "task-2299"} { // one on each page
		kept = append(kept, seedRollout(t, s, "sp-"+task, "agent-a", enum.RolloutIntentNoop, task))
	}
	lost := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")

	r.tick(context.Background())

	want := []proxy.TaskFilter{{Limit: 1000}, {Limit: 1000, Cursor: "task-0999"}, {Limit: 1000, Cursor: "task-1999"}}
	if !slices.Equal(fp.lists, want) {
		t.Fatalf("agent listed with %+v, want %+v", fp.lists, want)
	}
	for _, ro := range kept {
		if !unchanged(t, s, ro) {
			t.Fatalf("rollout %s changed though its agent holds its task", ro.ID())
		}
	}
	if !requeued(t, s, lost) {
		t.Fatal("the task missing from the whole listing was not queued for a reinstall")
	}
}

// A listing that stops short says nothing about the tasks it did not reach.
// Whatever stops it, the budget, a failed page, a cursor that comes back, pages
// of different snapshots or a cancelled tick, no rollout is judged missing: not
// the task on a page it never got, nor the one the agent lost. An agent that
// answers is logged once, with the cause; one that goes silent or a tick that is
// cancelled say nothing.
func TestIncompleteListMarksNothingMissing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pages    int           // list calls before the listing stops
		advance  time.Duration // moves the clock on each page
		failAt   int           // the page that fails with err
		err      error
		cursors  []string // the next cursor each page names, in turn
		versions []string // the resource version each page names, in turn
		cancelAt int      // the page during which the tick is cancelled
		cause    string   // the cause the warning names; none if empty
	}{
		{name: "the budget is used", pages: 1, advance: defaultPushTimeout, cause: causeBudget},
		{name: "a refused page uses the budget before it is asked again", pages: 1, advance: defaultPushTimeout, failAt: 1, err: tooLarge(), cause: causeBudget},
		{name: "a page with too many elements uses the budget before it is asked again", pages: 1, advance: defaultPushTimeout, failAt: 1, err: tooManyElementsErr(), cause: causeBudget},
		{name: "a page is answered with an error", pages: 2, failAt: 2, err: httpAnswer(proxy.ErrListTasks, 500, errkind.Internal), cause: causeAnswer},
		{name: "a page is unanswered", pages: 2, failAt: 2, err: noAnswer(proxy.ErrListTasks)},
		{name: "the cursor repeats", pages: 2, cursors: []string{"a", "a"}, cause: causeCursor},
		{name: "the cursors cycle", pages: 3, cursors: []string{"a", "b", "a"}, cause: causeCursor},
		{name: "the snapshot changes", pages: 2, versions: []string{"rv-1", "rv-2"}, cause: causeSnapshot},
		{name: "the last page is of another snapshot", pages: 3, versions: []string{"rv-1", "rv-1", "rv-2"}, cause: causeSnapshot},
		{name: "the tick is cancelled", pages: 1, cancelAt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &testClock{now: time.Now()}
			fp := &fakeProxy{}
			r, s, _ := newFakeRunner(t, fp)
			var logs bytes.Buffer
			r.logger = zerolog.New(&logs)
			r.clock = clock.Now
			holdN(fp, 2300)
			fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
				call := len(fp.lists)
				clock.advance(tc.advance)
				if call == tc.cancelAt {
					cancel()
				}
				if call <= len(tc.cursors) {
					page.NextCursor = tc.cursors[call-1]
				}
				if call <= len(tc.versions) {
					page.ResourceVersion = tc.versions[call-1]
				}
				if call == tc.failAt {
					return tc.err
				}
				return nil
			}
			rollouts := []*model.Rollout{
				seedRollout(t, s, "sp-first", "agent-a", enum.RolloutIntentNoop, "task-0000"),
				seedRollout(t, s, "sp-last", "agent-a", enum.RolloutIntentNoop, "task-2299"),
				seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost"),
			}

			r.tick(ctx)

			if len(fp.lists) != tc.pages {
				t.Fatalf("agent got %d list calls, want %d", len(fp.lists), tc.pages)
			}
			for _, ro := range rollouts {
				if !unchanged(t, s, ro) {
					t.Fatalf("rollout %s changed on an incomplete listing", ro.ID())
				}
			}
			warns := strings.Count(logs.String(), `"level":"warn"`)
			if (tc.cause == "") != (warns == 0) || warns > 1 || !strings.Contains(logs.String(), tc.cause) {
				t.Fatalf("logged %d warnings, want one naming %q or none: %s", warns, tc.cause, logs.String())
			}
		})
	}
}

// Pages that name one snapshot complete the listing, and so do pages that name
// none: an agent need not say which snapshot a page is of.
func TestPagesOfOneSnapshotComplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions []string // the resource version each page names, in turn
	}{
		{"no versions", nil},
		{"one version on every page", []string{"rv-1", "rv-1", "rv-1"}},
		{"a page without a version", []string{"rv-1", "", "rv-1"}},
		{"the first page without a version", []string{"", "rv-1", "rv-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{}
			r, s, _ := newFakeRunner(t, fp)
			var logs bytes.Buffer
			r.logger = zerolog.New(&logs)
			holdN(fp, 2300)
			fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
				if call := len(fp.lists); call <= len(tc.versions) {
					page.ResourceVersion = tc.versions[call-1]
				}
				return nil
			}
			kept := seedRollout(t, s, "sp-last", "agent-a", enum.RolloutIntentNoop, "task-2299")
			lost := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")

			r.tick(context.Background())

			if len(fp.lists) != 3 {
				t.Fatalf("agent got %d list calls, want 3", len(fp.lists))
			}
			if !unchanged(t, s, kept) || !requeued(t, s, lost) {
				t.Fatal("the listing was complete: want the lost task queued for a reinstall and the other rollout left alone")
			}
			if logs.Len() != 0 {
				t.Fatalf("a listing that came whole logged: %s", logs.String())
			}
		})
	}
}

// A listing that fails is sorted as a GetTask that failed was: an agent that
// gave no answer backs off, one that answered, whatever the status, does not.
// A 404 on the listing is such an answer, not a lost task: only a task missing
// from a complete listing is queued for a reinstall. A page refused as too large
// is an answer too: it is asked again smaller before the listing is given up, as is
// one with too many elements.
func TestListErrorsKeepTheObservationClasses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		calls      int  // list calls: a refused page is asked again, ten sizes down to one task
		unanswered bool // the agent backs off
		requeued   bool // the rollout of the task the agent lost is queued for a reinstall
	}{
		{"listed, a task is missing", nil, 1, false, true},
		{"no response", agentErr{errkind.Internal, fmt.Errorf("%w: %w: EOF", proxy.ErrListTasks, proxy.ErrRequest)}, 1, true, false},
		{"deadline exceeded", agentErr{errkind.DeadlineExceeded, fmt.Errorf("%w: rpc error: code = DeadlineExceeded", proxy.ErrListTasks)}, 1, true, false},
		{"context deadline exceeded", agentErr{errkind.Internal, fmt.Errorf("%w: %w", proxy.ErrListTasks, context.DeadlineExceeded)}, 1, true, false},
		{"net.Error: connection refused", fmt.Errorf("%w: %w", proxy.ErrListTasks, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), 1, true, false},
		{"net.Error: connection reset mid-body", fmt.Errorf("%w: %w: %w", proxy.ErrListTasks, proxy.ErrDecode, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}), 1, true, false},
		{"answered 503", httpAnswer(proxy.ErrListTasks, 503, errkind.Unavailable), 1, false, false},
		{"answered 500", httpAnswer(proxy.ErrListTasks, 500, errkind.Internal), 1, false, false},
		{"answered 404", httpAnswer(proxy.ErrListTasks, 404, errkind.NotFound), 1, false, false},
		{"answered grpc resource exhausted", agentErr{errkind.Unavailable, fmt.Errorf("%w: rpc error: code = ResourceExhausted desc = retained task limit reached", proxy.ErrListTasks)}, 1, false, false},
		{"answer malformed", agentErr{errkind.Internal, fmt.Errorf("%w: %w: unexpected end of JSON input", proxy.ErrListTasks, proxy.ErrDecode)}, 1, false, false},
		{"answer refused as too large", tooLarge(), 10, false, false},
		{"answer refused for too many elements", tooManyElementsErr(), 10, false, false},
		{"answer with a task of too many conditions", tooManyConditionsErr(), 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{listErr: tc.err}
			r, s, _ := newFakeRunner(t, fp)
			kept := seedRollout(t, s, "sp-kept", "agent-a", enum.RolloutIntentNoop, "task-kept")
			lost := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")
			fp.hold("task-kept")

			r.tick(context.Background())

			if len(fp.lists) != tc.calls {
				t.Fatalf("agent got %d list calls, want %d", len(fp.lists), tc.calls)
			}
			b, backedOff := r.backoffs["agent-a"]
			if backedOff != tc.unanswered || (backedOff && b.failures != 1) {
				t.Fatalf("backoff %+v (set %v), want set %v with one failure", b, backedOff, tc.unanswered)
			}
			if !unchanged(t, s, kept) {
				t.Fatal("rollout of a task the agent holds changed")
			}
			if got := requeued(t, s, lost); got != tc.requeued {
				t.Fatalf("lost task requeued = %v, want %v", got, tc.requeued)
			}
			if !tc.requeued && !unchanged(t, s, lost) {
				t.Fatal("rollout changed on a failed listing")
			}
		})
	}
}

// A page the proxy refuses as too large, or as holding more elements than it
// builds, is asked again from the same cursor with half the limit, and the smaller
// limit holds for the rest of the turn: an agent that can be listed at some size is
// listed whole, and its lost task found. A page of 1000 tasks that is over the
// budget of elements while half of it fits is such an agent. The next turn starts at
// 1000 again.
func TestRefusedPagesAreAskedAgainSmaller(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal func() error
		fits    int // the most tasks a page may hold before the agent's answer is refused
	}{
		{"an answer too large, a page of 250 fits", tooLarge, 300},
		{"an answer with too many elements, a page of 250 fits", tooManyElementsErr, 300},
		{"an answer with too many elements, a page of 500 fits", tooManyElementsErr, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{}
			r, s, _ := newFakeRunner(t, fp)
			var logs bytes.Buffer
			r.logger = zerolog.New(&logs)
			holdN(fp, 2300)
			fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
				if len(page.Tasks) > tc.fits {
					return tc.refusal()
				}
				return nil
			}
			kept := []*model.Rollout{
				seedRollout(t, s, "sp-first", "agent-a", enum.RolloutIntentNoop, "task-0000"),
				seedRollout(t, s, "sp-last", "agent-a", enum.RolloutIntentNoop, "task-2299"),
			}
			lost := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")

			r.tick(context.Background())

			// Every size from 1000 down is asked from the first page until one fits;
			// the pages of that size follow, each from the cursor of the one before.
			var want []proxy.TaskFilter
			limit := 1000
			for ; limit > tc.fits; limit /= 2 {
				want = append(want, proxy.TaskFilter{Limit: limit})
			}
			want = append(want, proxy.TaskFilter{Limit: limit})
			for last := limit - 1; last < 2300; last += limit {
				want = append(want, proxy.TaskFilter{Limit: limit, Cursor: fmt.Sprintf("task-%04d", last)})
			}
			if !slices.Equal(fp.lists, want) {
				t.Fatalf("agent listed with %+v, want %+v", fp.lists, want)
			}
			for _, ro := range kept {
				if !unchanged(t, s, ro) {
					t.Fatalf("rollout %s changed though its agent holds its task", ro.ID())
				}
			}
			if !requeued(t, s, lost) {
				t.Fatal("the listing was complete: want the lost task queued for a reinstall")
			}
			if logs.Len() != 0 {
				t.Fatalf("a listing that came whole logged: %s", logs.String())
			}
			if b, ok := r.backoffs["agent-a"]; ok {
				t.Fatalf("the agent answered, but is backed off: %+v", b)
			}

			// A new turn starts over at 1000.
			fp.lists = nil
			fp.applyResp = []string{"task-lost"} // the reinstall
			r.tick(context.Background())
			if len(fp.lists) < 1 || fp.lists[0] != (proxy.TaskFilter{Limit: 1000}) {
				t.Fatalf("next turn started with %+v, want {Limit: 1000}", fp.lists)
			}
		})
	}
}

// A task that is refused even alone, for its size or for the elements it holds,
// leaves the listing incomplete, and no rollout is judged missing: not the task on
// the pages that came, the task that cannot be listed, nor the one the agent lost.
// The refusals are answers.
func TestATaskRefusedEvenAloneLeavesTheListingIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal func() error
		cause   string
	}{
		{"a task too large", tooLarge, causeTooLarge},
		{"a task with too many elements", tooManyElementsErr, causeElements},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProxy{}
			r, s, _ := newFakeRunner(t, fp)
			var logs bytes.Buffer
			r.logger = zerolog.New(&logs)
			holdN(fp, 2300)
			fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
				for _, task := range page.Tasks {
					if task.ID == "task-0500" {
						return tc.refusal()
					}
				}
				return nil
			}
			rollouts := []*model.Rollout{
				seedRollout(t, s, "sp-before", "agent-a", enum.RolloutIntentNoop, "task-0000"),
				seedRollout(t, s, "sp-large", "agent-a", enum.RolloutIntentNoop, "task-0500"),
				seedRollout(t, s, "sp-after", "agent-a", enum.RolloutIntentNoop, "task-2299"),
				seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost"),
			}

			r.tick(context.Background())

			// 1000 is refused, 500 is the page before the task, then every size from
			// the cursor after it down to one task is refused.
			want := []proxy.TaskFilter{{Limit: 1000}, {Limit: 500}}
			for _, limit := range []int{500, 250, 125, 62, 31, 15, 7, 3, 1} {
				want = append(want, proxy.TaskFilter{Limit: limit, Cursor: "task-0499"})
			}
			if !slices.Equal(fp.lists, want) {
				t.Fatalf("agent listed with %+v, want %+v", fp.lists, want)
			}
			for _, ro := range rollouts {
				if !unchanged(t, s, ro) {
					t.Fatalf("rollout %s changed on an incomplete listing", ro.ID())
				}
			}
			if b, ok := r.backoffs["agent-a"]; ok {
				t.Fatalf("the agent answered, but is backed off: %+v", b)
			}
			if warns := strings.Count(logs.String(), `"level":"warn"`); warns != 1 || !strings.Contains(logs.String(), tc.cause) {
				t.Fatalf("logged %d warnings, want one naming %q: %s", warns, tc.cause, logs.String())
			}
		})
	}
}

// A task with more conditions than a task may hold is refused in any page that holds
// it, so the runner does not ask for a smaller page: the listing ends at the first
// call, incomplete, with a cause of its own, and no rollout is judged missing. This is
// the difference from a page over the budget of elements, which is asked again
// smaller and listed whole when the halves fit (TestRefusedPagesAreAskedAgainSmaller).
func TestATaskWithTooManyConditionsEndsTheListingAtOnce(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	var logs bytes.Buffer
	r.logger = zerolog.New(&logs)
	holdN(fp, 2300)
	fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
		for _, task := range page.Tasks {
			if task.ID == "task-0500" {
				return tooManyConditionsErr()
			}
		}
		return nil
	}
	rollouts := []*model.Rollout{
		seedRollout(t, s, "sp-before", "agent-a", enum.RolloutIntentNoop, "task-0000"),
		seedRollout(t, s, "sp-bad", "agent-a", enum.RolloutIntentNoop, "task-0500"),
		seedRollout(t, s, "sp-after", "agent-a", enum.RolloutIntentNoop, "task-2299"),
		seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost"),
	}

	r.tick(context.Background())

	if want := []proxy.TaskFilter{{Limit: 1000}}; !slices.Equal(fp.lists, want) {
		t.Fatalf("agent listed with %+v, want one call, %+v", fp.lists, want)
	}
	for _, ro := range rollouts {
		if !unchanged(t, s, ro) {
			t.Fatalf("rollout %s changed on an incomplete listing", ro.ID())
		}
	}
	if b, ok := r.backoffs["agent-a"]; ok {
		t.Fatalf("the agent answered, but is backed off: %+v", b)
	}
	if got := r.incomplete["agent-a"]; got != causeConditions {
		t.Fatalf("the listing is incomplete for %q, want %q", got, causeConditions)
	}
	if warns := strings.Count(logs.String(), `"level":"warn"`); warns != 1 || !strings.Contains(logs.String(), causeConditions) {
		t.Fatalf("logged %d warnings, want one naming %q: %s", warns, causeConditions, logs.String())
	}
}

// An agent whose tasks cannot be listed completely is logged when that begins,
// with the cause, and when it ends, naming the latest cause: not on every tick it
// goes on, nor when its cause changes meanwhile.
func TestListingChangesAreLoggedOnce(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	var logs bytes.Buffer
	r.logger = zerolog.New(&logs)
	fp.hold(taskIDs(seedRollouts(t, s, "agent-a", 1, enum.RolloutIntentNoop))...)
	logged := func(level string) int { return strings.Count(logs.String(), `"level":"`+level+`"`) }

	fp.listErr = httpAnswer(proxy.ErrListTasks, 500, errkind.Internal)
	r.tick(context.Background())
	r.tick(context.Background())
	if logged("warn") != 1 || logged("info") != 0 || !strings.Contains(logs.String(), causeAnswer) {
		t.Fatalf("two incomplete ticks logged %d warnings and %d infos, want 1 and 0, the warning naming %q: %s", logged("warn"), logged("info"), causeAnswer, logs.String())
	}

	// The cause changes while the listing stays incomplete: silently.
	fp.listErr = nil
	fp.onList = func(_ context.Context, page *proxyv1.ListTasksResponse) error {
		page.NextCursor = "stuck"
		return nil
	}
	r.tick(context.Background())
	if logged("warn") != 1 || logged("info") != 0 {
		t.Fatalf("a change of cause logged %d warnings and %d infos in all, want 1 and 0: %s", logged("warn"), logged("info"), logs.String())
	}

	fp.onList = nil
	r.tick(context.Background())
	r.tick(context.Background())
	if logged("warn") != 1 || logged("info") != 1 || !strings.Contains(logs.String(), `"was":"`+causeCursor+`"`) {
		t.Fatalf("a complete tick after them logged %d warnings and %d infos in all, want 1 and 1, the info naming %q: %s", logged("warn"), logged("info"), causeCursor, logs.String())
	}
}

// The state of an agent's listing is forgotten with the agent's rollouts, as its
// backoff is: when the agent comes back with the same trouble, that is news again.
func TestListingStateForgetsAgentsWithoutRollouts(t *testing.T) {
	fp := &fakeProxy{listErr: httpAnswer(proxy.ErrListTasks, 500, errkind.Internal)}
	r, s, _ := newFakeRunner(t, fp)
	var logs bytes.Buffer
	r.logger = zerolog.New(&logs)
	ro := seedRollout(t, s, "sp-live", "agent-a", enum.RolloutIntentNoop, "task-live")

	r.tick(context.Background())
	if len(r.incomplete) != 1 {
		t.Fatalf("incomplete listing not noted: %v", r.incomplete)
	}
	if err := s.DeleteRollout(context.Background(), ro.ID()); err != nil {
		t.Fatal(err)
	}
	r.tick(context.Background())
	if len(r.incomplete) != 0 {
		t.Fatalf("state of an agent without rollouts kept: %v", r.incomplete)
	}

	_ = seedRollout(t, s, "sp-live", "agent-a", enum.RolloutIntentNoop, "task-live")
	r.tick(context.Background())
	if warns := strings.Count(logs.String(), `"level":"warn"`); warns != 2 {
		t.Fatalf("logged %d warnings, want 2: one each time the agent began to fail: %s", warns, logs.String())
	}
}

// A rollout that changes while its agent is being listed is left as the change
// made it, though the listing lacks its task: the new intent decides what
// happens to the task, and a rollout deleted meanwhile does not come back. The
// next tick does what the change asks: it deletes the task, applies the spec, or
// makes no call.
func TestObserveLeavesRolloutsChangedDuringTheListing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*model.Rollout) // nil: the rollout is deleted
		intent  enum.RolloutIntent
		deletes []string // the tasks the next tick deletes
		applies []string // the specs the next tick applies
	}{
		{"uninstall requested", func(ro *model.Rollout) { ro.SetIntent(enum.RolloutIntentUninstall); ro.MarkPending(1) }, enum.RolloutIntentUninstall, []string{"task-lost"}, nil},
		{"deployed again", func(ro *model.Rollout) { ro.SetIntent(enum.RolloutIntentUpdate); ro.MarkPending(2) }, enum.RolloutIntentUpdate, nil, []string{"sp-lost"}},
		{"deleted", nil, 0, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fp := &fakeProxy{applyResp: []string{"task-lost"}}
			r, s, _ := newFakeRunner(t, fp)
			ro := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")
			spec, err := s.GetSpec(ctx, "sp-lost")
			if err != nil {
				t.Fatal(err)
			}
			spec.BumpGeneration() // what a deploy does, so that generation 2 can be applied
			if err = s.UpsertSpec(ctx, spec); err != nil {
				t.Fatal(err)
			}
			fp.onList = func(context.Context, *proxyv1.ListTasksResponse) error {
				if tc.change == nil {
					return s.DeleteRollout(ctx, ro.ID())
				}
				current, err := s.GetRollout(ctx, ro.ID())
				if err != nil {
					return err
				}
				tc.change(current)
				return s.UpsertRollout(ctx, current)
			}

			r.tick(ctx)

			after, err := s.GetRollout(ctx, ro.ID())
			switch {
			case tc.change == nil && !errors.Is(err, storage.ErrNotFound):
				t.Fatalf("rollout deleted during the listing came back: %v", err)
			case tc.change != nil && (err != nil || after.Intent() != tc.intent || after.ActualTaskID() != "task-lost"):
				t.Fatalf("rollout changed during the listing was rewritten: err=%v intent=%s task=%q", err, after.Intent(), after.ActualTaskID())
			}

			fp.onList = nil
			r.tick(ctx)

			if applies := specOf(fp.applies); !slices.Equal(fp.deletes, tc.deletes) || !slices.Equal(applies, tc.applies) {
				t.Fatalf("next tick deleted %v and applied %v, want %v and %v", fp.deletes, applies, tc.deletes, tc.applies)
			}
		})
	}
}

// A synced rollout without a task has nothing to look up: its agent gets no call.
func TestObserveWithoutATaskMakesNoCall(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	ro := seedRollout(t, s, "sp-bare", "agent-a", enum.RolloutIntentNoop, "")

	r.tick(context.Background())

	if len(fp.lists) != 0 {
		t.Fatalf("agent got %d list calls, want none", len(fp.lists))
	}
	if !unchanged(t, s, ro) {
		t.Fatal("rollout without a task changed")
	}
}

// The budget is the turn's, and pushes come first in it: when they use it up,
// the agent is not listed that tick.
func TestPushesThatUseTheBudgetLeaveNoTimeToObserve(t *testing.T) {
	clock := &testClock{now: time.Now()}
	fp := &fakeProxy{applyResp: []string{"task-new"}}
	r, s, _ := newFakeRunner(t, fp)
	r.clock = clock.Now
	fp.onApply = func() { clock.advance(r.cfg.PushTimeout) }
	synced := seedRollout(t, s, "sp-synced", "agent-a", enum.RolloutIntentNoop, "task-synced")
	pending := seedRollout(t, s, "sp-pending", "agent-a", enum.RolloutIntentInstall, "")

	r.tick(context.Background())

	if len(fp.applies) != 1 || len(fp.lists) != 0 {
		t.Fatalf("applies=%d lists=%d, want 1 and 0: the push used the turn's budget", len(fp.applies), len(fp.lists))
	}
	after, err := s.GetRollout(context.Background(), pending.ID())
	if err != nil || after.Status() != enum.SyncStatusSynced {
		t.Fatalf("pending rollout not completed (err=%v)", err)
	}
	if !unchanged(t, s, synced) {
		t.Fatal("synced rollout changed though its agent was not listed")
	}
}

// A page that comes back whole as its deadline passes is an answer: the listing
// goes on, and what it shows is acted on. The turn's budget runs on a clock that
// stands still, so that only the page's own deadline passes.
func TestPageAnsweredAtItsDeadlineCounts(t *testing.T) {
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	now := time.Now()
	r.clock = func() time.Time { return now }
	r.cfg.PushTimeout = 20 * time.Millisecond
	fp.onList = func(ctx context.Context, _ *proxyv1.ListTasksResponse) error {
		<-ctx.Done()
		return nil
	}
	kept := seedRollout(t, s, "sp-kept", "agent-a", enum.RolloutIntentNoop, "task-kept")
	lost := seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")
	fp.hold("task-kept")

	r.tick(context.Background())

	if !unchanged(t, s, kept) || !requeued(t, s, lost) {
		t.Fatal("the page came back whole at its deadline: want the lost task queued for a reinstall and the other rollout left alone")
	}
	if b, ok := r.backoffs["agent-a"]; ok {
		t.Fatalf("the agent answered, but is backed off: %+v", b)
	}
}
