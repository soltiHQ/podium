package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	proxyv1 "github.com/soltiHQ/control-plane/api/proxy/v1"
)

type aggregateFixture struct {
	pages []*TaskRunPage
	calls int
}

func (f *aggregateFixture) ListTaskRunsPage(_ context.Context, _ string, q TaskRunQuery) (*TaskRunPage, error) {
	if q.Limit != 1000 {
		return nil, fmt.Errorf("unexpected limit: %d", q.Limit)
	}
	f.calls++
	if f.calls > len(f.pages) {
		return nil, fmt.Errorf("unexpected request")
	}
	return f.pages[f.calls-1], nil
}

func aggregatePage(n int, next string) *TaskRunPage {
	rows := make([]proxyv1.TaskRun, n)
	for i := range rows {
		rows[i] = proxyv1.TaskRun{Generation: 2, Attempt: i + 1, Status: "succeeded", FinishedAtPresent: true, WorkloadAPIVersion: "solti.io/v1", WorkloadKind: "Subprocess"}
	}
	return &TaskRunPage{TaskUID: "uid", ResourceVersion: "opaque-rv", NextCursor: next, Runs: rows}
}

func TestLiveRunAggregatePreservesMoreThan500AndBoundary1024(t *testing.T) {
	f := &aggregateFixture{pages: []*TaskRunPage{aggregatePage(1000, "next"), aggregatePage(24, "")}}
	got, err := aggregateTaskRuns(context.Background(), f, "task")
	if err != nil || len(got.Runs) != 1024 || f.calls != 2 {
		t.Fatalf("got=%+v err=%v calls=%d", got, err, f.calls)
	}
	if got.TaskUID != "uid" || got.Runs[1023].WorkloadKind != "Subprocess" || got.Runs[1023].WorkloadAPIVersion != "solti.io/v1" || !got.Runs[1023].FinishedAtPresent {
		t.Fatal("lost run identity metadata")
	}
}

func TestLiveRunAggregateRejectsRowPayloadPageAndSnapshotBounds(t *testing.T) {
	cases := map[string]struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{
		"rows":            {[]*TaskRunPage{aggregatePage(1000, "next"), aggregatePage(25, "")}, "1024 runs", 2},
		"full-with-more":  {[]*TaskRunPage{aggregatePage(1000, "next"), aggregatePage(24, "more")}, "1024 runs", 2},
		"source-page":     {[]*TaskRunPage{aggregatePage(1001, "")}, "requested 1000 rows", 1},
		"repeated-cursor": {[]*TaskRunPage{aggregatePage(1, "next"), aggregatePage(1, "next")}, "repeated cursor", 2},
		"missing":         {[]*TaskRunPage{nil}, "snapshot changed or missing", 1},
	}
	changed := aggregatePage(1, "")
	changed.ResourceVersion = "different"
	cases["changed-rv"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{[]*TaskRunPage{aggregatePage(1, "next"), changed}, "snapshot changed or missing", 2}
	changedUID := aggregatePage(1, "")
	changedUID.TaskUID = "different"
	cases["changed-uid"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{[]*TaskRunPage{aggregatePage(1, "next"), changedUID}, "snapshot changed or missing", 2}
	missingUID := aggregatePage(0, "")
	missingUID.TaskUID = ""
	cases["missing-uid"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{[]*TaskRunPage{missingUID}, "snapshot changed or missing", 1}
	missingRV := aggregatePage(0, "")
	missingRV.ResourceVersion = ""
	cases["missing-rv"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{[]*TaskRunPage{missingRV}, "snapshot changed or missing", 1}
	payload := aggregatePage(3, "")
	for i := range payload.Runs {
		payload.Runs[i].Error = strings.Repeat("x", 3*1024*1024)
	}
	cases["payload"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{[]*TaskRunPage{payload}, "8 MiB", 1}
	pages := make([]*TaskRunPage, 16)
	for i := range pages {
		pages[i] = aggregatePage(0, fmt.Sprint(i))
	}
	cases["pages"] = struct {
		pages []*TaskRunPage
		want  string
		calls int
	}{pages, "16 pages", 16}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &aggregateFixture{pages: c.pages}
			got, err := aggregateTaskRuns(context.Background(), f, "task")
			if got != nil || !errors.Is(err, ErrListTaskRuns) || !strings.Contains(err.Error(), c.want) || f.calls != c.calls {
				t.Fatalf("partial=%+v err=%v calls=%d", got, err, f.calls)
			}
		})
	}
}

func TestLiveRunAggregateCanceledBeforeTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &aggregateFixture{}
	got, err := aggregateTaskRuns(ctx, f, "task")
	if got != nil || !errors.Is(err, context.Canceled) || f.calls != 0 {
		t.Fatalf("got=%+v err=%v calls=%d", got, err, f.calls)
	}
}
