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
	if entity.CreatedAt().IsZero() {
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
// Pagination ordering is (CreatedAt DESC, ID ASC).
// Cursor is an opaque token produced by this backend; a malformed cursor returns ErrInvalidArgument.
func (s *GenericStore[T]) List(ctx context.Context, predicate func(T) bool, opts storage.ListOptions) (*storage.ListResult[T], error) {
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}

	limit := storage.NormalizeLimit(opts.Limit)

	s.mu.RLock()
	if len(s.data) == 0 {
		s.mu.RUnlock()
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	// The matches are the stored values themselves, sorted and searched under the
	// read lock; only the page is cloned.
	matches := make([]T, 0, len(s.data))
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
			matches = append(matches, entity)
		}
	}

	if len(matches) == 0 {
		s.mu.RUnlock()
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	sort.Slice(matches, func(i, j int) bool { return listLess(matches[i], matches[j]) })

	start := 0
	if opts.Cursor != "" {
		start, err = findCursorPosition(ctx, matches, cur)
		if err != nil {
			s.mu.RUnlock()
			return nil, err
		}
	}

	if start >= len(matches) {
		s.mu.RUnlock()
		return &storage.ListResult[T]{Items: []T{}, NextCursor: ""}, nil
	}

	end := start + limit
	if end > len(matches) {
		end = len(matches)
	}

	page := make([]T, 0, end-start)
	for _, entity := range matches[start:end] {
		page = append(page, entity.Clone())
	}
	more := end < len(matches)
	s.mu.RUnlock()

	var nextCursor string
	if more {
		last := page[len(page)-1]
		nextCursor, err = encodeCursor(cursor{
			CreatedAtUnixNano: last.CreatedAt().UnixNano(),
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

// All returns clones of the entities pred accepts, in one pass. Draining List
// page by page scans and sorts the matches again for every page; All does each once.
//
//   - A nil pred accepts every entity; otherwise pred runs once per entity, under the read lock.
//   - The result is sorted once by less. A nil less is the order List uses (CreatedAt DESC, ID ASC),
//     so All returns what draining List returns.
//   - The result is nil when nothing matches, as when draining an empty List.
//
// All takes no context: the stores, which have one, call all.
func (s *GenericStore[T]) All(pred func(T) bool, less func(a, b T) bool) []T {
	out, _ := s.all(context.Background(), pred, less)
	return out
}

// all is All for a caller with a context. Like List it looks at ctx every 1000
// entities and, once ctx is done, stops and returns its error.
func (s *GenericStore[T]) all(ctx context.Context, pred func(T) bool, less func(a, b T) bool) ([]T, error) {
	s.mu.RLock()
	out := make([]T, 0, len(s.data))
	i := 0
	for _, entity := range s.data {
		if i%1000 == 0 {
			if err := ctx.Err(); err != nil {
				s.mu.RUnlock()
				return nil, err
			}
		}
		i++

		if pred == nil || pred(entity) {
			out = append(out, entity.Clone())
		}
	}
	s.mu.RUnlock()

	if len(out) == 0 {
		return nil, nil
	}
	// The listing order is called directly, where a given less is an indirect call
	// per comparison, and every caller but a test asks for the listing order.
	if less == nil {
		sort.Slice(out, func(i, j int) bool { return listLess(out[i], out[j]) })
	} else {
		sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	}
	return out, nil
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

// listLess is the order of every listing: CreatedAt DESC, then ID ASC. CreatedAt is
// compared in UnixNano, the clock persisted cursors use.
func listLess[T domain.Entity[T]](a, b T) bool {
	ta, tb := a.CreatedAt().UnixNano(), b.CreatedAt().UnixNano()
	if ta != tb {
		return ta > tb
	}
	return a.ID() < b.ID()
}

// findCursorPosition returns the index of the first item strictly after the cursor under ordering (CreatedAt DESC, ID ASC).
func findCursorPosition[T domain.Entity[T]](ctx context.Context, items []T, cur cursor) (int, error) {
	for i, e := range items {
		if i%1000 == 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		}

		eu := e.CreatedAt().UnixNano()
		if eu == cur.CreatedAtUnixNano && e.ID() == cur.ID {
			return i + 1, nil
		}
		if eu < cur.CreatedAtUnixNano {
			return i, nil
		}
		if eu == cur.CreatedAtUnixNano && e.ID() > cur.ID {
			return i, nil
		}
	}
	return len(items), nil
}
