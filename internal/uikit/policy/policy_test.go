package policy

import (
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/event"
)

func id(userID string, perms ...enum.Permission) *identity.Identity {
	return &identity.Identity{UserID: userID, Permissions: perms}
}

func TestBuildNav(t *testing.T) {
	t.Run("nil identity is all-false", func(t *testing.T) {
		if got := BuildNav(nil); got != (Nav{}) {
			t.Fatalf("BuildNav(nil) = %+v, want zero", got)
		}
	})

	t.Run("any users permission shows the users menu", func(t *testing.T) {
		if got := BuildNav(id("u1", usersDelete)); !got.ShowUsers {
			t.Fatalf("ShowUsers = false, want true (usersDelete should suffice)")
		}
	})

	t.Run("full flag mapping", func(t *testing.T) {
		got := BuildNav(id("u1", usersAdd, specsGet, specsAdd, agentsGet))
		want := Nav{
			ShowUsers:   true, // usersAdd
			ShowTasks:   true, // specsGet
			ShowAgents:  true, // agentsGet
			ShowHistory: true, // agentsGet
			CanAddUser:  true, // usersAdd
			CanAddSpec:  true, // specsAdd
		}
		if got != want {
			t.Fatalf("BuildNav = %+v, want %+v", got, want)
		}
	})

	t.Run("no permissions is all-false", func(t *testing.T) {
		if got := BuildNav(id("u1")); got != (Nav{}) {
			t.Fatalf("BuildNav(no perms) = %+v, want zero", got)
		}
	})
}

func TestBuildUserDetail(t *testing.T) {
	t.Run("nil identity is all-false", func(t *testing.T) {
		if got := BuildUserDetail(nil, "target", true); got != (UserDetail{}) {
			t.Fatalf("BuildUserDetail(nil) = %+v, want zero", got)
		}
	})

	t.Run("other user within the ceiling: actions follow permissions", func(t *testing.T) {
		got := BuildUserDetail(id("admin", usersEdit, usersDelete), "victim", true)
		want := UserDetail{IsSelf: false, CanEdit: true, CanEditRoles: true, CanDelete: true, CanRevokeSessions: true}
		if got != want {
			t.Fatalf("BuildUserDetail(other) = %+v, want %+v", got, want)
		}
	})

	t.Run("other user above the ceiling: no actions", func(t *testing.T) {
		if got := BuildUserDetail(id("manager", usersEdit, usersDelete), "admin", false); got != (UserDetail{}) {
			t.Fatalf("BuildUserDetail(stronger user) = %+v, want zero", got)
		}
	})

	t.Run("self-guard: cannot delete or change own roles even with permission", func(t *testing.T) {
		got := BuildUserDetail(id("me", usersEdit, usersDelete), "me", true)
		want := UserDetail{
			IsSelf:            true,
			CanEdit:           true,  // editing own profile is allowed
			CanEditRoles:      false, // forced off when self
			CanDelete:         false, // forced off when self
			CanRevokeSessions: true,  // own sessions
		}
		if got != want {
			t.Fatalf("BuildUserDetail(self) = %+v, want %+v", got, want)
		}
	})

	t.Run("without users:edit nothing is editable", func(t *testing.T) {
		if got := BuildUserDetail(id("viewer", usersGet), "other", true); got != (UserDetail{}) {
			t.Fatalf("BuildUserDetail(viewer) = %+v, want zero", got)
		}
	})
}

func TestBuildAgentDetail(t *testing.T) {
	if got := BuildAgentDetail(nil); got != (AgentDetail{}) {
		t.Fatalf("BuildAgentDetail(nil) = %+v, want zero", got)
	}
	if got := BuildAgentDetail(id("u1", agentsEdit)); !got.CanEditLabels {
		t.Fatalf("CanEditLabels = false, want true")
	}
	if got := BuildAgentDetail(id("u1", agentsGet)); got.CanEditLabels {
		t.Fatalf("CanEditLabels = true with only agentsGet, want false")
	}
}

func TestBuildSpecDetail(t *testing.T) {
	if got := BuildSpecDetail(nil); got != (SpecDetail{}) {
		t.Fatalf("BuildSpecDetail(nil) = %+v, want zero", got)
	}

	got := BuildSpecDetail(id("u1", specsEdit))
	if !got.CanEdit || got.CanDelete {
		t.Fatalf("specsEdit should grant CanEdit but not CanDelete, got %+v", got)
	}
	if got.CanDeploy {
		t.Fatalf("CanDeploy = true without specsDeploy, want false")
	}
	if got = BuildSpecDetail(id("u1", specsDelete)); !got.CanDelete || got.CanEdit {
		t.Fatalf("specsDelete should grant CanDelete only, got %+v", got)
	}

	if got = BuildSpecDetail(id("u1", specsDeploy)); !got.CanDeploy {
		t.Fatalf("specsDeploy should grant CanDeploy, got %+v", got)
	}
}

func TestPlacementPolicyRequiresSpecAndAgentRead(t *testing.T) {
	for _, tc := range []struct {
		permissions []enum.Permission
		want        bool
	}{{nil, false}, {[]enum.Permission{specsGet}, false}, {[]enum.Permission{agentsGet}, false}, {[]enum.Permission{specsGet, agentsEdit}, false}, {[]enum.Permission{specsGet, agentsGet}, true}} {
		if got := BuildSpecDetail(id("reader", tc.permissions...)).CanViewPlacement; got != tc.want {
			t.Fatalf("%v: %v", tc.permissions, got)
		}
	}
}

func TestArchiveNavigationRequiresAgentRead(t *testing.T) {
	if BuildNav(id("editor", agentsEdit)).ShowHistory || BuildAgentDetail(id("editor", agentsEdit)).CanViewHistory {
		t.Fatal("agent edit exposes archive without read")
	}
	if !BuildNav(id("reader", agentsGet)).ShowHistory || !BuildAgentDetail(id("reader", agentsGet)).CanViewHistory {
		t.Fatal("agent read missing archive navigation")
	}
}

func TestBuildDashboardCanDismiss(t *testing.T) {
	if BuildDashboard(nil).CanDismiss(event.AgentInactive) {
		t.Fatal("nil identity can dismiss")
	}
	for _, tc := range []struct {
		perms []enum.Permission
		kind  string
		want  bool
	}{
		{[]enum.Permission{agentsEdit}, event.AgentInactive, true},
		{[]enum.Permission{agentsEdit}, event.AgentDisconnected, true},
		{[]enum.Permission{agentsEdit}, event.AgentDeleted, true},
		{[]enum.Permission{agentsGet, specsGet, usersGet}, event.AgentInactive, false},
		{[]enum.Permission{specsEdit}, event.SyncFailed, true},
		{[]enum.Permission{agentsEdit, usersEdit}, event.SyncFailed, false},
		{[]enum.Permission{usersEdit}, event.RateLimited, true},
		{[]enum.Permission{agentsEdit, specsEdit}, event.RateLimited, false},
		{[]enum.Permission{agentsEdit, specsEdit}, "not_a_kind", false},
		{[]enum.Permission{agentsEdit, specsEdit, usersEdit}, "not_a_kind", true},
	} {
		if got := BuildDashboard(id("viewer", tc.perms...)).CanDismiss(tc.kind); got != tc.want {
			t.Errorf("%v dismiss %s = %v, want %v", tc.perms, tc.kind, got, tc.want)
		}
	}
}
