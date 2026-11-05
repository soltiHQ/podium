package sync

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// servePage is what an agent answers one ListTasks call with: the names of the
// tasks on the page, the cursor of the next page ("" for the last) or an error.
// The first call is call 1.
type servePage func(call int, asked proxy.TaskFilter) (names []string, next string, err error)

// serve makes the agent of fp answer each ListTasks call as page says.
func serve(fp *fakeProxy, page servePage) {
	fp.onList = func(_ context.Context, resp *proxyv1.ListTasksResponse) error {
		call := len(fp.lists)
		names, next, err := page(call, fp.lists[call-1])
		if err != nil {
			return err
		}
		resp.Tasks = resp.Tasks[:0]
		for _, name := range names {
			resp.Tasks = append(resp.Tasks, proxyv1.Task{ID: name, UID: "uid-" + name})
		}
		resp.NextCursor = next
		return nil
	}
}

func taskName(i int) string { return fmt.Sprintf("task-%06d", i) }

// cursorAt is the cursor of the page that starts at task i.
func cursorAt(i int) string { return "at-" + strconv.Itoa(i) }

// startOf is the index of the first task of the page that asked wants.
func startOf(asked proxy.TaskFilter) int {
	i, _ := strconv.Atoi(strings.TrimPrefix(asked.Cursor, "at-"))
	return i
}

// held is an agent with total tasks, task-000000 on, that serves per to a page at
// most, and no more than it is asked for.
func held(total, per int) servePage {
	return func(_ int, asked proxy.TaskFilter) ([]string, string, error) {
		from := startOf(asked)
		to := min(from+min(per, asked.Limit), total)
		names := make([]string, 0, to-from)
		for i := from; i < to; i++ {
			names = append(names, taskName(i))
		}
		if to == total {
			return names, "", nil
		}
		return names, cursorAt(to), nil
	}
}

// The agents below misbehave without end. Each gives up long after any bound, and
// ends its listing, so that a runner without the bounds finishes its test rather
// than hangs it.
const giveUpAfter = 3 * 1024

// freshCursors has no tasks and names a new cursor on every page.
func freshCursors(call int, _ proxy.TaskFilter) ([]string, string, error) {
	if call > giveUpAfter {
		return nil, "", nil
	}
	return nil, cursorAt(call), nil
}

// refusesAllButOne refuses every page of more than one task as too large, and
// serves one fresh task and a fresh cursor on every other.
func refusesAllButOne(call int, asked proxy.TaskFilter) ([]string, string, error) {
	switch {
	case asked.Limit > 1:
		return nil, "", tooLarge()
	case call > giveUpAfter:
		return []string{taskName(call)}, "", nil
	}
	return []string{taskName(call)}, cursorAt(call), nil
}

// overfull serves one task more than it is asked for, from task-000000 on.
func overfull(_ int, asked proxy.TaskFilter) ([]string, string, error) {
	names := make([]string, 0, asked.Limit+1)
	for i := 0; i <= asked.Limit; i++ {
		names = append(names, taskName(i))
	}
	return names, "", nil
}

// heavyNames are task names that add up to size bytes: task-000000, then names of a
// mebibyte, the last of what is left.
func heavyNames(size int) []string {
	names := []string{taskName(0)}
	left := size - len(names[0])
	for i := 1; left > 0; i++ {
		n := min(left, 1<<20)
		prefix := strconv.Itoa(i) + "-"
		names = append(names, prefix+strings.Repeat("x", n-len(prefix)))
		left -= n
	}
	return names
}

// serves is an agent that holds the named tasks and serves per of them to a page.
func serves(names []string, per int) servePage {
	return func(_ int, asked proxy.TaskFilter) ([]string, string, error) {
		from := startOf(asked)
		to := min(from+per, len(names))
		if to == len(names) {
			return names[from:to], "", nil
		}
		return names[from:to], cursorAt(to), nil
	}
}

// boundsRun is a runner over an agent that serves pages as a test says, with the
// rollouts of tasks the agent holds and the rollout of one it lost.
type boundsRun struct {
	fp   *fakeProxy
	r    *Runner
	s    storage.Storage
	logs *bytes.Buffer
	kept []*model.Rollout
	lost *model.Rollout
}

// newBoundsRun gives the agent a rollout for each task named in kept.
func newBoundsRun(t *testing.T, page servePage, kept ...string) *boundsRun {
	t.Helper()
	fp := &fakeProxy{}
	r, s, _ := newFakeRunner(t, fp)
	b := &boundsRun{fp: fp, r: r, s: s, logs: new(bytes.Buffer)}
	r.logger = zerolog.New(b.logs)
	serve(fp, page)
	for i, task := range kept {
		b.kept = append(b.kept, seedRollout(t, s, fmt.Sprintf("sp-kept-%d", i), "agent-a", enum.RolloutIntentNoop, task))
	}
	b.lost = seedRollout(t, s, "sp-lost", "agent-a", enum.RolloutIntentNoop, "task-lost")
	return b
}

func (b *boundsRun) logged(level string) int {
	return strings.Count(b.logs.String(), `"level":"`+level+`"`)
}

// A listing is bounded in what one page may hold, in the length of a cursor, in the
// task IDs it keeps and their bytes, and in the calls it takes, refused pages
// included. An agent that breaks a bound is answering, so it is not backed off; but
// its listing is incomplete, and nothing is judged by it: not the task on the first
// page, nor the one the agent lost. One warning names the cause, and no task ID: an
// ID can be as long as a page.
func TestListingsThatBreakABoundJudgeNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		page  servePage
		calls int // list calls before the listing stops
		cause string
	}{
		{"a page holds more tasks than asked", overfull, 1, causeMoreTasks},
		{"the proxy refuses a page of more tasks than asked", func(int, proxy.TaskFilter) ([]string, string, error) {
			return nil, "", tooManyItems()
		}, 1, causeMoreTasks},
		{"a page holds more tasks than the smaller limit asked", func(call int, asked proxy.TaskFilter) ([]string, string, error) {
			if asked.Limit > 500 {
				return nil, "", tooLarge()
			}
			return overfull(call, asked)
		}, 2, causeMoreTasks},
		{"a cursor of 4097 bytes", func(int, proxy.TaskFilter) ([]string, string, error) {
			return []string{taskName(0)}, strings.Repeat("c", 4097), nil
		}, 1, causeLongCursor},
		{"a fresh cursor on every page, forever", freshCursors, 1024, causeListSize},
		{"fresh tasks on every page, three times the bound", held(3*65536, 1000), 66, causeListSize},
		{"65537 tasks", held(65537, 1000), 66, causeListSize},
		{"one byte of task IDs over the bound, across pages", serves(heavyNames(maxListedIDBytes+1), 3), 11, causeListSize},
		{"1025 pages", held(1025, 1), 1024, causeListSize},
		{"refused pages are pages", refusesAllButOne, 1024, causeListSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoundsRun(t, tc.page, taskName(0))

			b.r.tick(context.Background())

			if len(b.fp.lists) != tc.calls {
				t.Fatalf("agent got %d list calls, want %d", len(b.fp.lists), tc.calls)
			}
			for _, ro := range append(b.kept, b.lost) {
				if !unchanged(t, b.s, ro) {
					t.Fatalf("rollout %s changed on an incomplete listing", ro.ID())
				}
			}
			if backoff, ok := b.r.backoffs["agent-a"]; ok {
				t.Fatalf("the agent answered, but is backed off: %+v", backoff)
			}
			if got := b.r.incomplete["agent-a"]; got != tc.cause {
				t.Fatalf("the listing is incomplete for %q, want %q", got, tc.cause)
			}
			if warns := b.logged("warn"); warns != 1 || !strings.Contains(b.logs.String(), tc.cause) {
				t.Fatalf("logged %d warnings, want one naming %q: %s", warns, tc.cause, b.logs.String())
			}
			if size := b.logs.Len(); size > 1<<10 {
				t.Fatalf("one warning took %d bytes of log: is a task ID in it?", size)
			}
		})
	}
}

// The bounds are inclusive: 65536 task IDs, 32 MiB of them, 1024 calls and a cursor
// of 4096 bytes are listed whole, and a task missing from them is found, as in any
// complete listing. The last task is among those kept, so none was cut off.
func TestListingsAtTheirBoundsAreComplete(t *testing.T) {
	heavy := heavyNames(maxListedIDBytes)
	for _, tc := range []struct {
		name  string
		page  servePage
		calls int
		last  string // the last task the agent lists
	}{
		{"65536 tasks", held(65536, 1000), 66, taskName(65535)},
		{"32 MiB of task IDs", serves(heavy, 3), 11, heavy[len(heavy)-1]},
		{"1024 pages", held(1024, 1), 1024, taskName(1023)},
		{"a cursor of 4096 bytes", func(call int, _ proxy.TaskFilter) ([]string, string, error) {
			if call == 1 {
				return []string{taskName(0)}, strings.Repeat("c", 4096), nil
			}
			return []string{taskName(1)}, "", nil
		}, 2, taskName(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoundsRun(t, tc.page, taskName(0), tc.last)

			b.r.tick(context.Background())

			if len(b.fp.lists) != tc.calls {
				t.Fatalf("agent got %d list calls, want %d", len(b.fp.lists), tc.calls)
			}
			for _, ro := range b.kept {
				if !unchanged(t, b.s, ro) {
					t.Fatalf("rollout %s changed though its agent holds its task", ro.ID())
				}
			}
			if !requeued(t, b.s, b.lost) {
				t.Fatal("the listing was complete: want the lost task queued for a reinstall")
			}
			if b.logs.Len() != 0 {
				t.Fatalf("a listing that came whole logged: %s", b.logs.String())
			}
		})
	}
}

// A listing that breaks a bound is logged as any other incomplete one is: once
// when it begins, not on every tick it goes on, and once more, naming the cause,
// when the agent can be listed whole again.
func TestAListingOverTheBoundsIsLoggedOnce(t *testing.T) {
	b := newBoundsRun(t, freshCursors, taskName(0))

	b.r.tick(context.Background())
	b.r.tick(context.Background())
	if b.logged("warn") != 1 || b.logged("info") != 0 || !strings.Contains(b.logs.String(), causeListSize) {
		t.Fatalf("two incomplete ticks logged %d warnings and %d infos, want 1 and 0, the warning naming %q: %s", b.logged("warn"), b.logged("info"), causeListSize, b.logs.String())
	}

	serve(b.fp, held(2, 1000))
	b.r.tick(context.Background())
	if b.logged("warn") != 1 || b.logged("info") != 1 || !strings.Contains(b.logs.String(), `"was":"`+causeListSize+`"`) {
		t.Fatalf("a complete tick after them logged %d warnings and %d infos in all, want 1 and 1, the info naming %q: %s", b.logged("warn"), b.logged("info"), causeListSize, b.logs.String())
	}
}
