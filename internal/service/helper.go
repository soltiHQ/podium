package service

import "github.com/soltiHQ/control-plane/internal/storage"

// NormalizeListLimit clamps the requested page size to a safe range.
//
// If qlimit ≤ 0, the caller-provided default (dlimit) is used.
// If the result exceeds [storage.MaxListLimit], it is capped to that ceiling.
func NormalizeListLimit(qlimit, dlimit int) int {
	if qlimit <= 0 {
		qlimit = dlimit
	}
	if qlimit > storage.MaxListLimit {
		qlimit = storage.MaxListLimit
	}
	return qlimit
}
