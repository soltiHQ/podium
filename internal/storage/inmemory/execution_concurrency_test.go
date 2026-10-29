package inmemory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

func TestExecutionConcurrentFirstWriterRemainsImmutable(t *testing.T) {
	s := New()
	base := archiveRow(t, 1)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := base.Params()
			p.Error = fmt.Sprintf("result-%d", i)
			p.CapturedAt = p.CapturedAt.Add(time.Duration(i) * time.Second)
			if i%2 == 0 {
				p.Status = "failed"
			}
			code := int32(i)
			p.ExitCode = &code
			e, err := model.NewExecutionRecord(p)
			if err != nil {
				t.Error(err)
				return
			}
			<-start
			if err := s.UpsertExecution(context.Background(), e); err != nil {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	first, err := s.GetExecution(context.Background(), base.ID())
	if err != nil {
		t.Fatal(err)
	}
	// Whichever caller wins may differ, but a later conflicting replay must
	// preserve that complete result, not combine fields from competing writers.
	p := base.Params()
	p.Error = "later replacement"
	later, _ := model.NewExecutionRecord(p)
	if err := s.UpsertExecution(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetExecution(context.Background(), base.ID())
	if first.Error() != after.Error() || first.Status() != after.Status() || *first.ExitCode() != *after.ExitCode() || !first.CapturedAt().Equal(after.CapturedAt()) {
		t.Fatal("first terminal result changed")
	}
	var winner int
	if _, err := fmt.Sscanf(first.Error(), "result-%d", &winner); err != nil {
		t.Fatal(err)
	}
	if int(*first.ExitCode()) != winner || !first.CapturedAt().Equal(base.CapturedAt().Add(time.Duration(winner)*time.Second)) {
		t.Fatal("competing records mixed fields")
	}
}
