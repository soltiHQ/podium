package identity

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
)

func TestIdentity_HasPermission(t *testing.T) {
	t.Run("nil identity", func(t *testing.T) {
		var id *Identity
		if id.HasPermission(enum.Permission("read:any")) {
			t.Fatal("expected false for nil identity")
		}
	})

	t.Run("empty permission", func(t *testing.T) {
		id := &Identity{Permissions: []enum.Permission{"read:any"}}
		if id.HasPermission("") {
			t.Fatal("expected false for empty permission")
		}
	})

	t.Run("no permissions", func(t *testing.T) {
		id := &Identity{}
		if id.HasPermission(enum.Permission("read:any")) {
			t.Fatal("expected false when identity has no permissions")
		}
	})

	t.Run("permission not found", func(t *testing.T) {
		id := &Identity{Permissions: []enum.Permission{"read:any", "write:any"}}
		if id.HasPermission(enum.Permission("delete:any")) {
			t.Fatal("expected false when permission is not present")
		}
	})

	t.Run("permission found", func(t *testing.T) {
		id := &Identity{Permissions: []enum.Permission{"read:any", "write:any"}}
		if !id.HasPermission(enum.Permission("write:any")) {
			t.Fatal("expected true when permission is present")
		}
	})

	t.Run("duplicate permissions still true", func(t *testing.T) {
		id := &Identity{Permissions: []enum.Permission{"read:any", "read:any"}}
		if !id.HasPermission(enum.Permission("read:any")) {
			t.Fatal("expected true when permission is present (even duplicated)")
		}
	})
}
