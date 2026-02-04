package authenticator

import (
	"sort"

	"github.com/soltiHQ/control-plane/domain"
)

func collectPermissions(roles []*domain.RoleModel) []string {
	var (
		set = make(map[string]struct{}, 64)
	)
	for _, r := range roles {
		if r == nil {
			continue
		}
		for _, p := range r.PermissionsAll() {
			set[string(p)] = struct{}{}
		}
	}

	var (
		out = make([]string, 0, len(set))
	)
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
