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
	return &GenericStore[T]{
		data: make(map[string]T),
	}
}

// Upsert inserts or fully replaces an entity.
//
// The entity is deep-cloned before storage to prevent external mutations.
// Returns storage.ErrInvalidArgument if the entity has empty ID.
func (s *GenericStore[T]) Upsert(_ context.Context, entity T) error {
	id := entity.ID()
	if id == "" {
		return storage.ErrInvalidArgument
	}

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

// List retrieves entities with optional filtering and cursor-based pagination.
//
// Filtering:
//   - Pass nil predicate to retrieve all entities.
//   - Pass a function that returns true for entities to include.
//
// Pagination:
//   - Results are ordered by (UpdatedAt DESC, ID ASC) for stable cursor navigation.
//   - Invalid or corrupted cursors return storage.ErrInvalidArgument.
//
// All returned entities are deep clones isolated from the internal state.
func (s *GenericStore[T]) List(_ context.Context, predicate func(T) bool, opts storage.ListOptions) (*ListResult[T], error) {
	cur, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 || limit > storage.MaxListLimit {
		limit = storage.DefaultListLimit
	}
	s.mu.RLock()
	if len(s.data) == 0 {
		s.mu.RUnlock()
		return &ListResult[T]{
			Items:      nil,
			NextCursor: "",
		}, nil
	}
	snapshot := make([]T, 0, len(s.data))
	for _, entity := range s.data {
		if predicate == nil || predicate(entity) {
			snapshot = append(snapshot, entity.Clone())
		}
	}
	s.mu.RUnlock()

	sort.Slice(snapshot, func(i, j int) bool {
		ti, tj := snapshot[i].UpdatedAt(), snapshot[j].UpdatedAt()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return snapshot[i].ID() < snapshot[j].ID()
	})

	start := 0
	if opts.Cursor != "" {
		for i, entity := range snapshot {
			if entity.UpdatedAt().Equal(cur.UpdatedAt) && entity.ID() == cur.ID {
				start = i + 1
				break
			}
			if entity.UpdatedAt().Before(cur.UpdatedAt) {
				start = i
				break
			}
		}
	}

	end := start + limit + 1
	if end > len(snapshot) {
		end = len(snapshot)
	}
	page := snapshot[start:end]

	var nextCursor string
	if len(page) > limit {
		lastInPage := page[limit-1]
		nextCursor = encodeCursor(cursor{
			UpdatedAt: lastInPage.UpdatedAt(),
			ID:        lastInPage.ID(),
		})
		page = page[:limit]
	}
	return &ListResult[T]{
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
