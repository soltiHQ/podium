package apimap

import (
	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/model"
)

func Role(r *model.Role) restv1.Role {
	if r == nil {
		return restv1.Role{}
	}
	return restv1.Role{
		ID:   r.ID(),
		Name: r.Name(),
	}
}
