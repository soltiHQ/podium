package user

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

const defaultListLimit = 60

// ErrStale reports an update whose IfMatch names no tag of the stored user: the
// user changed since the caller read it. Nothing is written.
var ErrStale = errors.New("user changed since it was read")

// ListQuery describes a paginated users listing request.
type ListQuery struct {
	// Criteria is turned into the backend's filter by the store.
	Criteria storage.UserQueryCriteria

	Cursor string
	Limit  int
}

// Page is a paginated users listing result.
type Page struct {
	Items      []*model.User
	NextCursor string
}

// UpdatePatch is a partial change to a stored user: a nil field keeps the
// stored value, a set one replaces it. Disabled is not part of it (see
// SetDisabled). IfMatch is not a change but a condition on the stored user.
type UpdatePatch struct {
	// Name, Email and Subject are normalized like on Create.
	Name, Email, Subject *string

	// RoleIDs and Permissions replace the whole set, so a pointer to an empty
	// list clears it.
	RoleIDs, Permissions *[]string

	// IfMatch holds the ETags (see ETag) of the user as the caller read it. Nil
	// applies the patch to the stored user as it is. Otherwise the user the
	// transaction reads must have one of them, else nothing is written and
	// Update returns ErrStale. An empty list names no tag, so it always does.
	IfMatch []string
}

// apply sets the fields p names on u.
func (p UpdatePatch) apply(u *model.User) {
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Email != nil {
		u.SetEmail(*p.Email)
	}
	if p.Subject != nil {
		u.SetSubject(*p.Subject)
	}
	if p.RoleIDs != nil {
		u.SetRoleIDs(*p.RoleIDs)
	}
	if p.Permissions != nil {
		u.SetPermissions(*p.Permissions)
	}
}

// ETag returns the entity tag of u: the version of the user that a caller reads
// and later names in UpdatePatch.IfMatch. It is the lowercase hex of the first
// 16 bytes of SHA-256 over every stored field of u (ID, subject, email, name,
// disabled, role IDs, permissions, creation time and update time), strings and
// lists length prefixed, so two different users never share an encoding.
//
// It changes whenever any of those fields does, whichever write made the change.
// It does not rest on the update time alone: that moves only when a setter reads
// the clock, so two changes may share an instant, and a writer that sets the
// stamp (SetUpdatedAt) keeps whatever it sets. A write that stores the values
// already stored leaves it as it was. It is computed, never stored.
func ETag(u *model.User) string {
	b := appendString(nil, u.ID())
	b = appendString(b, u.Subject())
	b = appendString(b, u.Email())
	b = appendString(b, u.Name())
	if u.Disabled() {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	roles := u.RoleIDsAll()
	b = binary.AppendUvarint(b, uint64(len(roles)))
	for _, id := range roles {
		b = appendString(b, id)
	}
	perms := u.PermissionsAll()
	b = binary.AppendUvarint(b, uint64(len(perms)))
	for _, p := range perms {
		b = appendString(b, string(p))
	}
	b = appendTime(b, u.CreatedAt())
	b = appendTime(b, u.UpdatedAt())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// appendTime encodes the instant, not its location or monotonic reading, so a
// stamp read back from the log encodes as the one that was written.
func appendTime(b []byte, t time.Time) []byte {
	b = binary.AppendVarint(b, t.Unix())
	return binary.AppendVarint(b, int64(t.Nanosecond()))
}
