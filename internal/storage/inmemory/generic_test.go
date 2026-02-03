package inmemory

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// testEntity is a minimal implementation of domain.Entity[T] for tests.
type testEntity struct {
	id        string
	updatedAt time.Time
	meta      map[string]string
}

func (e testEntity) ID() string           { return e.id }
func (e testEntity) UpdatedAt() time.Time { return e.updatedAt }
func (e testEntity) Clone() testEntity {
	out := testEntity{
		id:        e.id,
		updatedAt: e.updatedAt,
	}
	if e.meta != nil {
		out.meta = make(map[string]string, len(e.meta))
		for k, v := range e.meta {
			out.meta[k] = v
		}
	}
	return out
}

func mustUpsert(t *testing.T, s *GenericStore[testEntity], e testEntity) {
	t.Helper()
	if err := s.Upsert(context.Background(), e); err != nil {
		t.Fatalf("Upsert() error: %v", err)
	}
}

func TestGenericStore_Upsert_EmptyID(t *testing.T) {
	s := NewGenericStore[testEntity]()
	err := s.Upsert(context.Background(), testEntity{id: ""})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got: %v", err)
	}
}

func TestGenericStore_Get_EmptyID(t *testing.T) {
	s := NewGenericStore[testEntity]()
	_, err := s.Get(context.Background(), "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got: %v", err)
	}
}

func TestGenericStore_Get_NotFound(t *testing.T) {
	s := NewGenericStore[testEntity]()
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestGenericStore_Delete_EmptyID(t *testing.T) {
	s := NewGenericStore[testEntity]()
	err := s.Delete(context.Background(), "")
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got: %v", err)
	}
}

func TestGenericStore_Delete_NotFound(t *testing.T) {
	s := NewGenericStore[testEntity]()
	err := s.Delete(context.Background(), "nope")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestGenericStore_Upsert_StoresDeepClone(t *testing.T) {
	s := NewGenericStore[testEntity]()
	now := time.Now()

	orig := testEntity{
		id:        "e1",
		updatedAt: now,
		meta:      map[string]string{"k": "v1"},
	}
	mustUpsert(t, s, orig)
	orig.meta["k"] = "mutated"

	got, err := s.Get(context.Background(), "e1")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.meta["k"] != "v1" {
		t.Fatalf("expected stored meta not to change, got: %q", got.meta["k"])
	}
}

func TestGenericStore_Get_ReturnsDeepClone(t *testing.T) {
	s := NewGenericStore[testEntity]()
	now := time.Now()

	mustUpsert(t, s, testEntity{
		id:        "e1",
		updatedAt: now,
		meta:      map[string]string{"k": "v1"},
	})
	got1, err := s.Get(context.Background(), "e1")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	got1.meta["k"] = "mutated"

	got2, err := s.Get(context.Background(), "e1")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got2.meta["k"] != "v1" {
		t.Fatalf("expected stored meta not to change after mutating returned clone, got: %q", got2.meta["k"])
	}
}

func TestGenericStore_Delete_Removes(t *testing.T) {
	s := NewGenericStore[testEntity]()
	now := time.Now()

	mustUpsert(t, s, testEntity{id: "e1", updatedAt: now})
	if err := s.Delete(context.Background(), "e1"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	_, err := s.Get(context.Background(), "e1")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got: %v", err)
	}
}

func TestGenericStore_List_Empty(t *testing.T) {
	s := NewGenericStore[testEntity]()
	res, err := s.List(context.Background(), nil, storage.ListOptions{})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil result")
	}
	if len(res.Items) != 0 {
		t.Fatalf("expected 0 items, got: %d", len(res.Items))
	}
	if res.NextCursor != "" {
		t.Fatalf("expected empty NextCursor, got: %q", res.NextCursor)
	}
}

func TestGenericStore_List_SortsByUpdatedAtDescThenIDAsc(t *testing.T) {
	s := NewGenericStore[testEntity]()

	t0 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 2, 1, 11, 0, 0, 0, time.UTC)
	mustUpsert(t, s, testEntity{id: "b", updatedAt: t1})
	mustUpsert(t, s, testEntity{id: "a", updatedAt: t1})
	mustUpsert(t, s, testEntity{id: "c", updatedAt: t0})

	res, err := s.List(context.Background(), nil, storage.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	gotIDs := make([]string, 0, len(res.Items))
	for _, it := range res.Items {
		gotIDs = append(gotIDs, it.ID())
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("unexpected order: got=%v want=%v", gotIDs, want)
	}
}

func TestGenericStore_List_PredicateFilters(t *testing.T) {
	s := NewGenericStore[testEntity]()
	now := time.Now()

	mustUpsert(t, s, testEntity{id: "a", updatedAt: now})
	mustUpsert(t, s, testEntity{id: "b", updatedAt: now})
	mustUpsert(t, s, testEntity{id: "c", updatedAt: now})

	res, err := s.List(context.Background(), func(e testEntity) bool {
		return e.ID() == "b" || e.ID() == "c"
	}, storage.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	var gotIDs []string
	for _, it := range res.Items {
		gotIDs = append(gotIDs, it.ID())
	}
	want := []string{"b", "c"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("unexpected filtered IDs: got=%v want=%v", gotIDs, want)
	}
}

func TestGenericStore_List_PaginationWithCursor(t *testing.T) {
	s := NewGenericStore[testEntity]()

	t0 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 2, 1, 11, 0, 0, 0, time.UTC)
	mustUpsert(t, s, testEntity{id: "c", updatedAt: t0})
	mustUpsert(t, s, testEntity{id: "b", updatedAt: t1})
	mustUpsert(t, s, testEntity{id: "a", updatedAt: t1})

	page1, err := s.List(context.Background(), nil, storage.ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List(page1) error: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("expected 2 items in page1, got %d", len(page1.Items))
	}
	if page1.Items[0].ID() != "a" || page1.Items[1].ID() != "b" {
		t.Fatalf("unexpected page1 IDs: %s, %s", page1.Items[0].ID(), page1.Items[1].ID())
	}
	if page1.NextCursor == "" {
		t.Fatalf("expected non-empty NextCursor on page1")
	}
	page2, err := s.List(context.Background(), nil, storage.ListOptions{
		Limit:  2,
		Cursor: page1.NextCursor,
	})
	if err != nil {
		t.Fatalf("List(page2) error: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("expected 1 item in page2, got %d", len(page2.Items))
	}
	if page2.Items[0].ID() != "c" {
		t.Fatalf("unexpected page2 ID: %s", page2.Items[0].ID())
	}
	if page2.NextCursor != "" {
		t.Fatalf("expected empty NextCursor on last page, got: %q", page2.NextCursor)
	}
}

func TestGenericStore_List_InvalidCursor(t *testing.T) {
	s := NewGenericStore[testEntity]()
	now := time.Now()

	mustUpsert(t, s, testEntity{id: "a", updatedAt: now})
	_, err := s.List(context.Background(), nil, storage.ListOptions{
		Limit:  10,
		Cursor: "definitely-not-a-valid-cursor",
	})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for invalid cursor, got: %v", err)
	}
}

func TestGenericStore_List_ContextCanceled_DuringSnapshot(t *testing.T) {
	s := NewGenericStore[testEntity]()

	mustUpsert(t, s, testEntity{id: "a", updatedAt: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.List(ctx, nil, storage.ListOptions{Limit: 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestGenericStore_List_ContextCanceled_BeforeSortChunks(t *testing.T) {
	s := NewGenericStore[testEntity]()

	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10001; i++ {
		id := "id-" + leftPadInt(i, 5)
		mustUpsert(t, s, testEntity{id: id, updatedAt: base})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.List(ctx, nil, storage.ListOptions{Limit: 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// leftPadInt is a tiny helper to make IDs lexicographically sortable (id-00001 etc).
func leftPadInt(n int, width int) string {
	var s []byte
	x := n
	if x == 0 {
		s = append(s, '0')
	} else {
		for x > 0 {
			s = append([]byte{byte('0' + (x % 10))}, s...)
			x /= 10
		}
	}
	for len(s) < width {
		s = append([]byte{'0'}, s...)
	}
	return string(s)
}
