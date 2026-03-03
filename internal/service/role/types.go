package role

import (
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const defaultListLimit = 30

// ListQuery describes a paginated role listing request.
type ListQuery struct {
	Filter storage.RoleFilter
	Cursor string
	Limit  int
}

// Page is a paginated role listing result.
type Page struct {
	Items      []*model.Role
	NextCursor string
}
