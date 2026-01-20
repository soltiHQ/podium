package storage

const (
	// DefaultListLimit defines the default page size when Limit is not specified or invalid.
	DefaultListLimit = 100
	// MaxListLimit defines the maximum allowed page size to prevent resource exhaustion.
	MaxListLimit = 500
)

// ListOptions specifies pagination parameters for list operations.
//
// Each storage implementation Cursor differently.
type ListOptions struct {
	// Cursor is an opaque continuation token returned from a previous list call.
	Cursor string
	// Limit specifies the maximum number of items to return.
	Limit int
}

// ListResult contains a page of results from a list operation.
type ListResult[T any] struct {
	// Items contain the requested page of items.
	// May be nil or empty if no items match the query.
	Items []T
	// NextCursor is an opaque token for retrieving the next page.
	// Empty string indicates this is the final page.
	NextCursor string
}
