package event

import (
	"sync"
	"testing"
)

func TestRing_AppendAndRecent(t *testing.T) {
	r := NewRing[int](5)

	r.Append(1)
	r.Append(2)
	r.Append(3)

	got := r.Recent(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got))
	}
	if got[0] != 3 || got[1] != 2 {
		t.Fatalf("expected [3,2], got %v", got)
	}
}

func TestRing_RecentReturnsAllWhenNExceedsLen(t *testing.T) {
	r := NewRing[int](10)

	r.Append(1)
	r.Append(2)

	got := r.Recent(100)
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got))
	}
}

func TestRing_RecentEmptyBuffer(t *testing.T) {
	r := NewRing[int](5)

	if got := r.Recent(5); got != nil {
		t.Fatalf("expected nil for empty buffer, got %v", got)
	}
}

func TestRing_RecentZeroN(t *testing.T) {
	r := NewRing[int](5)
	r.Append(1)

	if got := r.Recent(0); got != nil {
		t.Fatalf("expected nil for n=0, got %v", got)
	}
}

func TestRing_EvictsOldestWhenFull(t *testing.T) {
	r := NewRing[int](3)

	for i := 1; i <= 5; i++ {
		r.Append(i)
	}

	got := r.Recent(3)
	if len(got) != 3 {
		t.Fatalf("expected 3 items, got %d", len(got))
	}
	if got[0] != 5 || got[1] != 4 || got[2] != 3 {
		t.Fatalf("expected [5,4,3], got %v", got)
	}
}

func TestRing_DeleteFunc(t *testing.T) {
	r := NewRing[int](10)

	for i := 1; i <= 5; i++ {
		r.Append(i)
	}

	removed := r.DeleteFunc(func(v int) bool { return v%2 == 0 })
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}

	got := r.Recent(10)
	if len(got) != 3 {
		t.Fatalf("expected 3 remaining, got %d", len(got))
	}
	if got[0] != 5 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("expected [5,3,1], got %v", got)
	}
}

func TestRing_DeleteFuncNoMatch(t *testing.T) {
	r := NewRing[int](5)
	r.Append(1)
	r.Append(3)

	removed := r.DeleteFunc(func(v int) bool { return v%2 == 0 })
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	if got := r.Recent(10); len(got) != 2 {
		t.Fatalf("expected 2 items unchanged, got %d", len(got))
	}
}

func TestRing_ConcurrentAccess(t *testing.T) {
	r := NewRing[int](100)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := range 100 {
				r.Append(base*100 + j)
			}
		}(i)
	}
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				r.Recent(10)
			}
		}()
	}
	wg.Wait()
}
