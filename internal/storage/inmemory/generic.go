package inmemory

import (
	"context"
	"sort"
	"sync"

	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// GenericStore provides thread-safe in-memory CRUD operations for any domain.Entity type.
//
// Type parameter T must implement domain.Entity[T].
type GenericStore[T domain.Entity[T]] struct {
	mu   sync.RWMutex
	data map[string]T
}

// NewGenericStore creates an empty generic store for type T.
func NewGenericStore[T domain.Entity[T]]() *GenericStore[T] {
	return &GenericStore[T]{data: make(map[string]T)}
}

func validateEntity[T domain.Entity[T]](entity T) error {
	var zero T
	if any(entity) == any(zero) {
		return storage.ErrInvalidArgument
	}

	if entity.ID() == "" {
		return storage.ErrInvalidArgument
	}
	if entity.UpdatedAt().IsZero() {
		return storage.ErrInvalidArgument
	}
	return nil
}

// Create inserts a new entity and fails if it already exists.
//
// The entity is deep-cloned before storage to prevent external mutations.
// Returns storage.ErrInvalidArgument if the entity has empty ID or violates storage invariants.
// Returns storage.ErrAlreadyExists if the ID already exists.
func (s *GenericStore[T]) Create(_ context.Context, entity T) error {
	if err := validateEntity(entity); err != nil {
		return err
	}
	id := entity.ID()

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[id]; ok {
		return storage.ErrAlreadyExists
	}
	s.data[id] = entity.Clone()
	return nil
}

// Update loads an entity by id, applies fn to a cloned copy, and stores the result atomically.
//
// Returns storage.ErrInvalidArgument if id is empty or fn is nil.
// Returns storage.ErrNotFound if the entity doesn't exist.
func (s *GenericStore[T]) Update(_ context.Context, id string, fn func(cur T) (T, error)) error {
	if id == "" || fn == nil {
		return storage.ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.data[id]
	if !ok {
		return storage.ErrNotFound
	}

	next, err := fn(cur.Clone())
	if err != nil {
		return err
	}

	if err = validateEntity(next); err != nil {
		return err
	}
	if next.ID() != id {
		return storage.ErrInvalidArgument
	}

	s.data[id] = next.Clone()
	return nil
}

// Upsert inserts or fully replaces an entity.
//
// The entity is deep-cloned before storage to prevent external mutations.
// Returns storage.ErrInvalidArgument if the entity violates storage invariants.
func (s *GenericStore[T]) Upsert(_ context.Context, entity T) error {
	if err := validateEntity(entity); err != nil {
		return err
	}

	id := entity.ID()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.data[id] = entity.Clone()
	return nil
}

// Get retrieves an entity by ID.
//
// Returns a deep clone to prevent external mutations affecting the stored state.
// Returns storage.ErrNotFound if the entity doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *GenericStore[T]) Get(_ context.Context, id string) (T, error) {
	var zero T
	if id == "" {
		return zero, storage.ErrInvalidArgument
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entity, ok := s.data[id]
	if !ok {
		return zero, storage.ErrNotFound
	}
	return entity.Clone(), nil
}

// GetMany retrieves multiple entities by IDs in a single read lock.
//
// Semantics:
//   - Returns storage.ErrInvalidArgument if ids are empty or contain empty elements.
//   - Returns storage.ErrNotFound if any id is missing.
//   - Preserves the order of ids (caller can deduplicate before calling).
//   - Returns deep clones (same as Get).
func (s *GenericStore[T]) GetMany(_ context.Context, ids []string) ([]T, error) {
	if len(ids) == 0 {
		return nil, storage.ErrInvalidArgument
	}
	for _, id := range ids {
		if id == "" {
			return nil, storage.ErrInvalidArgument
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		entity, ok := s.data[id]
		if !ok {
			return nil, storage.ErrNotFound
		}
		out = append(out, entity.Clone())
	}
	return out, nil
}

// List retrieves entities with optional filtering and cursor-based pagination.
//
// Filtering:
//   - Pass nil predicate to retrieve all entities.
//   - Pass a function that returns true for entities to include.
//
// Pagination ordering is (UpdatedAt DESC, ID ASC).
// Cursor is an opaque token produced by this backend; a malformed cursor returns ErrInvalidArgument.
func (s *GenericStore[T]) List(ctx context.Context, predicate func(T) bool, opts storage.ListOptions) (*storage.ListResult[T], error) {
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}

	limit := storage.NormalizeLimit(opts.Limit)

	// snapshot under read lock
	s.mu.RLock()
	if len(s.data) == 0 {
		s.mu.RUnlock()
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	snapshot := make([]T, 0, len(s.data))
	i := 0
	for _, entity := range s.data {
		if i%1000 == 0 {
			select {
			case <-ctx.Done():
				s.mu.RUnlock()
				return nil, ctx.Err()
			default:
			}
		}
		i++

		if predicate == nil || predicate(entity) {
			snapshot = append(snapshot, entity.Clone())
		}
	}
	s.mu.RUnlock()

	if len(snapshot) == 0 {
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	// sort once (ctx cannot interrupt sort.Slice in the middle; acceptable for inmemory)
	sort.Slice(snapshot, func(i, j int) bool {
		ti, tj := snapshot[i].UpdatedAt(), snapshot[j].UpdatedAt()
		if !ti.Equal(tj) {
			return ti.After(tj) // DESC
		}
		return snapshot[i].ID() < snapshot[j].ID() // ASC
	})

	start := 0
	if opts.Cursor != "" {
		start, err = findCursorPosition(ctx, snapshot, cur)
		if err != nil {
			return nil, err
		}
	}

	if start >= len(snapshot) {
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	end := start + limit
	if end > len(snapshot) {
		end = len(snapshot)
	}

	page := snapshot[start:end]

	var nextCursor string
	if end < len(snapshot) {
		last := page[len(page)-1]
		nextCursor, err = encodeCursor(cursor{
			UpdatedAtUnixNano: last.UpdatedAt().UnixNano(),
			ID:                last.ID(),
		})
		if err != nil {
			return nil, err
		}
	}

	return &storage.ListResult[T]{
		Items:      page,
		NextCursor: nextCursor,
	}, nil
}

// Delete removes an entity by ID.
//
// Returns storage.ErrNotFound if the entity doesn't exist, storage.ErrInvalidArgument for empty IDs.
func (s *GenericStore[T]) Delete(_ context.Context, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.data, id)
	return nil
}

// findCursorPosition returns the index of the first item strictly after the cursor under ordering (UpdatedAt DESC, ID ASC).
func findCursorPosition[T domain.Entity[T]](ctx context.Context, items []T, cur cursor) (int, error) {
	for i, e := range items {
		if i%1000 == 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		}

		eu := e.UpdatedAt().UnixNano()

		// exact match -> start after it
		if eu == cur.UpdatedAtUnixNano && e.ID() == cur.ID {
			return i + 1, nil
		}

		// strictly after cursor in (DESC, ASC) order:
		//   UpdatedAt smaller => later
		//   UpdatedAt equal and ID greater => later
		if eu < cur.UpdatedAtUnixNano {
			return i, nil
		}
		if eu == cur.UpdatedAtUnixNano && e.ID() > cur.ID {
			return i, nil
		}
	}

	// the cursor is after all items -> nothing left
	return len(items), nil
}
