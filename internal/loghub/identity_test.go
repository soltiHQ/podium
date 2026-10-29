package loghub_test

import (
	"context"
	"sync"
	"testing"
	"time"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/internal/loghub"
)

func TestSameNameReplacementHasSeparateIdentitySource(t *testing.T) {
	var mu sync.Mutex
	sources := make(map[string]chan *taskv1.StreamTaskLogsResponse)
	opens := make(map[string]int)
	hub := loghub.NewIdentity(func(_ context.Context, agent, name, uid string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		if agent != "agent" || name != "name" {
			t.Errorf("wrong stream address: %s/%s", agent, name)
		}
		mu.Lock()
		defer mu.Unlock()
		opens[uid]++
		sources[uid] = make(chan *taskv1.StreamTaskLogsResponse, 1)
		return sources[uid], nil
	}, loghub.Options{})
	defer hub.Close()
	old, stopOld, err := hub.SubscribeIdentity(context.Background(), "agent", "name", "old-uid")
	if err != nil {
		t.Fatal(err)
	}
	alsoOld, stopAlsoOld, err := hub.SubscribeIdentity(context.Background(), "agent", "name", "old-uid")
	if err != nil {
		t.Fatal(err)
	}
	fresh, stopFresh, err := hub.SubscribeIdentity(context.Background(), "agent", "name", "new-uid")
	if err != nil {
		t.Fatal(err)
	}
	defer stopFresh()
	mu.Lock()
	if opens["old-uid"] != 1 || opens["new-uid"] != 1 {
		t.Fatalf("cache reused replacement or duplicated same UID: %+v", opens)
	}
	oldSource, freshSource := sources["old-uid"], sources["new-uid"]
	mu.Unlock()
	oldSource <- chunk("old process")
	freshSource <- chunk("new process")
	for _, check := range []struct {
		stream <-chan *taskv1.StreamTaskLogsResponse
		text   string
	}{{old, "old process"}, {alsoOld, "old process"}, {fresh, "new process"}} {
		select {
		case ev := <-check.stream:
			if string(ev.GetChunk().GetLine()) != check.text {
				t.Fatalf("stream crossed UID boundary: %v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("identity stream stalled")
		}
	}
	stopOld()
	stopAlsoOld()
	freshSource <- chunk("still running")
	select {
	case ev, ok := <-fresh:
		if !ok || string(ev.GetChunk().GetLine()) != "still running" {
			t.Fatal("old incarnation teardown closed replacement")
		}
	case <-time.After(time.Second):
		t.Fatal("replacement stalled after old teardown")
	}
}
