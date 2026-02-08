package inmemory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// testEntity is a minimal domain.Entity implementation for GenericStore tests.
type testEntity struct {
	id        string
	createdAt time.Time
	updatedAt time.Time
	payload   map[string]string
}

func newTestEntity(id string, t time.Time) *testEntity {
	return &testEntity{
		createdAt: t,
		updatedAt: t,
		id:        id,
		payload:   map[string]string{"k": "v"},
	}
}

func (e *testEntity) ID() string           { return e.id }
func (e *testEntity) CreatedAt() time.Time { return e.createdAt }
func (e *testEntity) UpdatedAt() time.Time { return e.updatedAt }
func (e *testEntity) Clone() *testEntity {
	if e == nil {
		return nil
	}
	cp := &testEntity{
		id:        e.id,
		createdAt: e.createdAt,
		updatedAt: e.updatedAt,
	}
	if e.payload != nil {
		cp.payload = make(map[string]string, len(e.payload))
		for k, v := range e.payload {
			cp.payload[k] = v
		}
	}
	return cp
}

func TestGenericStore_CreateGetDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	e := newTestEntity("a", time.Unix(10, 0).UTC())

	if err := s.Create(ctx, e); err != nil {
		t.Fatalf("Create() err=%v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() err=%v", err)
	}
	if got == nil || got.ID() != "a" {
		t.Fatalf("Get() got=%v", got)
	}

	got.payload["k"] = "mutated"
	got2, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() err=%v", err)
	}
	if got2.payload["k"] != "v" {
		t.Fatalf("expected stored entity to be immutable from outside, got payload=%v", got2.payload)
	}

	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete() err=%v", err)
	}
	_, err = s.Get(ctx, "a")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, err=%v", err)
	}
}

func TestGenericStore_CreateAlreadyExists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	e := newTestEntity("a", time.Unix(10, 0).UTC())
	if err := s.Create(ctx, e); err != nil {
		t.Fatalf("Create() err=%v", err)
	}
	if err := s.Create(ctx, e); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, err=%v", err)
	}
}

func TestGenericStore_ValidateEntity_UpdatedAtRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	e := &testEntity{id: "a", updatedAt: time.Time{}} // zero UpdatedAt
	if err := s.Create(ctx, e); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
	if err := s.Upsert(ctx, e); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

func TestGenericStore_UpsertReplace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	e1 := newTestEntity("a", time.Unix(10, 0).UTC())
	e2 := newTestEntity("a", time.Unix(20, 0).UTC())
	e2.payload["k"] = "v2"

	if err := s.Upsert(ctx, e1); err != nil {
		t.Fatalf("Upsert(e1) err=%v", err)
	}
	if err := s.Upsert(ctx, e2); err != nil {
		t.Fatalf("Upsert(e2) err=%v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() err=%v", err)
	}
	if !got.UpdatedAt().Equal(time.Unix(20, 0).UTC()) {
		t.Fatalf("expected UpdatedAt=20s, got=%v", got.UpdatedAt())
	}
	if got.payload["k"] != "v2" {
		t.Fatalf("expected payload v2, got=%v", got.payload)
	}
}

func TestGenericStore_Update(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	base := newTestEntity("a", time.Unix(10, 0).UTC())
	if err := s.Upsert(ctx, base); err != nil {
		t.Fatalf("Upsert() err=%v", err)
	}

	if err := s.Update(ctx, "a", nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for nil fn, err=%v", err)
	}

	err := s.Update(ctx, "a", func(cur *testEntity) (*testEntity, error) {
		cur.updatedAt = time.Unix(11, 0).UTC()
		cur.payload["k"] = "changed"
		return cur, nil
	})
	if err != nil {
		t.Fatalf("Update() err=%v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() err=%v", err)
	}
	if got.payload["k"] != "changed" {
		t.Fatalf("expected changed payload, got=%v", got.payload)
	}

	err = s.Update(ctx, "a", func(cur *testEntity) (*testEntity, error) {
		cur.id = "b"
		cur.updatedAt = time.Unix(12, 0).UTC()
		return cur, nil
	})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for id change, err=%v", err)
	}
}

func TestGenericStore_GetMany(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	if err := s.Upsert(ctx, newTestEntity("a", time.Unix(1, 0).UTC())); err != nil {
		t.Fatalf("Upsert(a) err=%v", err)
	}
	if err := s.Upsert(ctx, newTestEntity("b", time.Unix(2, 0).UTC())); err != nil {
		t.Fatalf("Upsert(b) err=%v", err)
	}

	items, err := s.GetMany(ctx, []string{"b", "a", "b"})
	if err != nil {
		t.Fatalf("GetMany() err=%v", err)
	}
	if len(items) != 3 || items[0].ID() != "b" || items[1].ID() != "a" || items[2].ID() != "b" {
		t.Fatalf("unexpected order/dups: %#v", []string{items[0].ID(), items[1].ID(), items[2].ID()})
	}

	_, err = s.GetMany(ctx, []string{"a", "missing"})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, err=%v", err)
	}
}

func TestGenericStore_List_OrderingAndCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	t1 := time.Unix(100, 0).UTC()
	t2 := time.Unix(200, 0).UTC()

	_ = s.Upsert(ctx, newTestEntity("c", t2))
	_ = s.Upsert(ctx, newTestEntity("b", t1))
	_ = s.Upsert(ctx, newTestEntity("a", t1))

	page1, err := s.List(ctx, nil, storage.ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List(page1) err=%v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page1.Items))
	}
	if page1.Items[0].ID() != "c" || page1.Items[1].ID() != "a" {
		t.Fatalf("unexpected page1 order: %q, %q", page1.Items[0].ID(), page1.Items[1].ID())
	}
	if page1.NextCursor == "" {
		t.Fatalf("expected non-empty NextCursor")
	}

	page2, err := s.List(ctx, nil, storage.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatalf("List(page2) err=%v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].ID() != "b" {
		t.Fatalf("unexpected page2: len=%d id=%v", len(page2.Items), func() string {
			if len(page2.Items) == 0 {
				return ""
			}
			return page2.Items[0].ID()
		}())
	}
	if page2.NextCursor != "" {
		t.Fatalf("expected empty NextCursor on last page, got %q", page2.NextCursor)
	}
}

func TestGenericStore_List_Filtering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewGenericStore[*testEntity]()

	_ = s.Upsert(ctx, newTestEntity("a", time.Unix(1, 0).UTC()))
	_ = s.Upsert(ctx, newTestEntity("b", time.Unix(2, 0).UTC()))
	_ = s.Upsert(ctx, newTestEntity("c", time.Unix(3, 0).UTC()))

	onlyB := func(e *testEntity) bool { return e.ID() == "b" }

	res, err := s.List(ctx, onlyB, storage.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List() err=%v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID() != "b" {
		t.Fatalf("unexpected filtered result: %+v", res.Items)
	}
}

func TestCursor_BackendValidation(t *testing.T) {
	t.Parallel()

	raw := cursor{
		Backend:           "not-inmemory",
		Version:           1,
		UpdatedAtUnixNano: time.Unix(10, 0).UTC().UnixNano(),
		ID:                "x",
	}

	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal err=%v", err)
	}

	s := base64.RawURLEncoding.EncodeToString(b)

	_, err = decodeCursor(s)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for foreign backend cursor, err=%v", err)
	}
}
