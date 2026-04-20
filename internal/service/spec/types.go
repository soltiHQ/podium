package spec

import (
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const defaultListLimit = 30

// ListQuery describes a paginated task spec listing request.
//
// Callers supply backend-agnostic `Criteria`; the service translates
// into a backend filter through the Storage interface. This keeps the
// HTTP handler from needing to import a concrete storage package just
// to build a filter.
type ListQuery struct {
	Criteria storage.SpecQueryCriteria
	Cursor   string
	Limit    int
}

// Page is a paginated task spec listing the result.
type Page struct {
	Items      []*model.Spec
	NextCursor string
}
