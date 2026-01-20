package inmemory

import "github.com/soltiHQ/control-plane/internal/storage"

// ListResult contains a page of results from an in-memory generic list operation.
//
// This is an internal alias used by GenericStore.
type ListResult[T any] = storage.ListResult[T]
