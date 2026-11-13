package inmemory

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

func TestAgentFilter_Matches_AllPredicatesANDed(t *testing.T) {
	t.Parallel()

	a := mkAgent(t, "a1")
	a.LabelAdd("env", "prod")
	a.LabelAdd("tier", "edge")

	if !NewAgentFilter().Matches(a) {
		t.Fatalf("empty filter must match")
	}

	if !NewAgentFilter().ByLabel("env", "prod").Matches(a) {
		t.Fatalf("expected match by label")
	}
	if NewAgentFilter().ByLabel("env", "dev").Matches(a) {
		t.Fatalf("expected no match by label value")
	}
	if NewAgentFilter().ByLabel("missing", "x").Matches(a) {
		t.Fatalf("expected no match for missing label")
	}

	f := NewAgentFilter().
		ByLabel("env", "prod").
		ByLabel("tier", "edge")
	if !f.Matches(a) {
		t.Fatalf("expected match for combined labels")
	}

	f2 := NewAgentFilter().
		ByLabel("env", "prod").
		ByLabel("tier", "core")
	if f2.Matches(a) {
		t.Fatalf("expected no match for ANDed predicates")
	}
}

func TestAgentFilter_StaleAtBefore(t *testing.T) {
	t.Parallel()

	now := time.Now()
	a := mkAgent(t, "a1")

	// Zero staleAt: always matches (safe default).
	if !NewAgentFilter().StaleAtBefore(now).Matches(a) {
		t.Fatalf("zero staleAt must match")
	}

	// staleAt in the past: matches.
	a.SetStaleAt(now.Add(-time.Minute))
	if !NewAgentFilter().StaleAtBefore(now).Matches(a) {
		t.Fatalf("past staleAt must match")
	}

	// staleAt in the future: does not match.
	a.SetStaleAt(now.Add(time.Minute))
	if NewAgentFilter().StaleAtBefore(now).Matches(a) {
		t.Fatalf("future staleAt must not match")
	}

	// Combined with another predicate.
	a.SetStaleAt(now.Add(-time.Minute))
	a.LabelAdd("env", "prod")
	f := NewAgentFilter().StaleAtBefore(now).ByLabel("env", "prod")
	if !f.Matches(a) {
		t.Fatalf("combined filter must match")
	}
}

func TestUserFilter_Matches_AllPredicatesANDed(t *testing.T) {
	t.Parallel()

	u := mkUser(t, "u1", "sub-1")
	u.SetEmail("sub-1@example.com")
	u.SetName("User 1")
	userAddRole(t, u, "r-admin")
	userAddPerm(t, u, enum.UsersGet)

	if !NewUserFilter().Matches(u) {
		t.Fatalf("empty filter must match")
	}

	if !NewUserFilter().ByEmail("sub-1@example.com").Matches(u) {
		t.Fatalf("expected match by email")
	}
	if NewUserFilter().ByEmail("x@example.com").Matches(u) {
		t.Fatalf("expected no match by email")
	}

	if !NewUserFilter().ByDisabled(false).Matches(u) {
		t.Fatalf("expected match by disabled=false")
	}
	u.Disable()
	if !NewUserFilter().ByDisabled(true).Matches(u) {
		t.Fatalf("expected match by disabled=true")
	}

	if !NewUserFilter().ByRoleID("r-admin").Matches(u) {
		t.Fatalf("expected match by role id")
	}
	if NewUserFilter().ByRoleID("r-viewer").Matches(u) {
		t.Fatalf("expected no match by role id")
	}

	if !NewUserFilter().ByPermission(enum.UsersGet).Matches(u) {
		t.Fatalf("expected match by permission")
	}
	if NewUserFilter().ByPermission(enum.UsersDelete).Matches(u) {
		t.Fatalf("expected no match by permission")
	}

	f := NewUserFilter().
		ByDisabled(true).
		ByEmail("sub-1@example.com").
		ByRoleID("r-admin").
		ByPermission(enum.UsersGet)
	if !f.Matches(u) {
		t.Fatalf("expected match for ANDed predicates")
	}

	f2 := NewUserFilter().
		ByDisabled(true).
		ByEmail("sub-1@example.com").
		ByRoleID("r-admin").
		ByPermission(enum.UsersDelete)
	if f2.Matches(u) {
		t.Fatalf("expected no match for ANDed predicates")
	}
}

func TestRoleFilter_Matches_AllPredicatesANDed(t *testing.T) {
	t.Parallel()

	r := mkRole(t, "r1", "admin")
	roleAddPerm(t, r, enum.UsersGet)
	roleAddPerm(t, r, enum.AgentsEdit)

	if !NewRoleFilter().Matches(r) {
		t.Fatalf("empty filter must match")
	}

	if !NewRoleFilter().ByName("admin").Matches(r) {
		t.Fatalf("expected match by name")
	}
	if NewRoleFilter().ByName("viewer").Matches(r) {
		t.Fatalf("expected no match by name")
	}

	if !NewRoleFilter().ByPermission(enum.UsersGet).Matches(r) {
		t.Fatalf("expected match by permission")
	}
	if NewRoleFilter().ByPermission(enum.UsersDelete).Matches(r) {
		t.Fatalf("expected no match by permission")
	}

	f := NewRoleFilter().
		ByName("admin").
		ByPermission(enum.AgentsEdit)
	if !f.Matches(r) {
		t.Fatalf("expected match for ANDed predicates")
	}

	f2 := NewRoleFilter().
		ByName("admin").
		ByPermission(enum.UsersDelete)
	if f2.Matches(r) {
		t.Fatalf("expected no match for ANDed predicates")
	}
}

func TestStore_List_FilterTypes_AreBackendSpecific(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := New()

	requireNoErr(t, s.UpsertAgent(ctx, mkAgent(t, "a1")))
	requireNoErr(t, s.UpsertUser(ctx, mkUser(t, "u1", "sub-1")))
	requireNoErr(t, s.UpsertRole(ctx, mkRole(t, "r1", "admin")))

	if _, err := s.ListAgents(ctx, NewAgentFilter(), storage.ListOptions{}); err != nil {
		t.Fatalf("expected no error, err=%v", err)
	}
	if _, err := s.ListUsers(ctx, NewUserFilter(), storage.ListOptions{}); err != nil {
		t.Fatalf("expected no error, err=%v", err)
	}
	if _, err := s.ListRoles(ctx, NewRoleFilter(), storage.ListOptions{}); err != nil {
		t.Fatalf("expected no error, err=%v", err)
	}

	type foreignAgentFilter struct{}
	type foreignUserFilter struct{}
	type foreignRoleFilter struct{}

	var af storage.AgentFilter = (*foreignAgentFilter)(nil)
	var uf storage.UserFilter = (*foreignUserFilter)(nil)
	var rf storage.RoleFilter = (*foreignRoleFilter)(nil)

	_, err := s.ListAgents(ctx, af, storage.ListOptions{})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	_, err = s.ListUsers(ctx, uf, storage.ListOptions{})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}

	_, err = s.ListRoles(ctx, rf, storage.ListOptions{})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, err=%v", err)
	}
}

// TestBuildAgentFilterMatchesBuilders pins the factory to the builder chain it
// composes: over one fixed set, every criteria value matches what the chain
// matches, and the set is the expected one, so neither can be wrong the same way.
func TestBuildAgentFilterMatchesBuilders(t *testing.T) {
	t.Parallel()

	now := time.Now()
	mk := func(id, name, endpoint string, staleAt time.Time) *model.Agent {
		a, err := model.NewAgent(id, name, endpoint)
		requireNoErr(t, err)
		a.SetStaleAt(staleAt)
		return a
	}
	agents := []*model.Agent{
		mk("alpha-id", "one", "http://h1", now.Add(-time.Hour)), // matches on ID, stale
		mk("a2", "Beta-Name", "http://h2", now.Add(time.Hour)),  // matches on name (mixed case), fresh
		mk("a3", "three", "http://gamma-host", time.Time{}),     // matches on endpoint, zero StaleAt
		mk("a4", "four", "http://h4", now.Add(-2*time.Hour)),    // matches no query, stale
	}

	cases := []struct {
		name string
		c    storage.AgentQueryCriteria
		f    func() *AgentFilter
		want []string
	}{
		{"empty", storage.AgentQueryCriteria{}, func() *AgentFilter { return NewAgentFilter() }, []string{"alpha-id", "a2", "a3", "a4"}},
		{"query id", storage.AgentQueryCriteria{Query: "alpha"}, func() *AgentFilter { return NewAgentFilter().Query("alpha") }, []string{"alpha-id"}},
		{"query name case-insensitive", storage.AgentQueryCriteria{Query: "bETa-nAmE"}, func() *AgentFilter { return NewAgentFilter().Query("bETa-nAmE") }, []string{"a2"}},
		{"query endpoint", storage.AgentQueryCriteria{Query: "gamma"}, func() *AgentFilter { return NewAgentFilter().Query("gamma") }, []string{"a3"}},
		{"query no match", storage.AgentQueryCriteria{Query: "zzz"}, func() *AgentFilter { return NewAgentFilter().Query("zzz") }, nil},
		{"stale bound", storage.AgentQueryCriteria{StaleAtBefore: now}, func() *AgentFilter { return NewAgentFilter().StaleAtBefore(now) }, []string{"alpha-id", "a3", "a4"}},
		{"both", storage.AgentQueryCriteria{Query: "a", StaleAtBefore: now}, func() *AgentFilter { return NewAgentFilter().Query("a").StaleAtBefore(now) }, []string{"alpha-id", "a3", "a4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := New().BuildAgentFilter(tc.c).(*AgentFilter)
			if !ok {
				t.Fatalf("BuildAgentFilter returned %T, want *AgentFilter", got)
			}
			ref := tc.f()
			var ids []string
			for _, a := range agents {
				if got.Matches(a) != ref.Matches(a) {
					t.Fatalf("agent %s: factory %v, builder chain %v", a.ID(), got.Matches(a), ref.Matches(a))
				}
				if got.Matches(a) {
					ids = append(ids, a.ID())
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("matched %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestBuildUserFilterMatchesBuilders(t *testing.T) {
	t.Parallel()

	users := []*model.User{
		mkUser(t, "u1", "alice"),
		mkUser(t, "u2", "bob"),
	}
	cases := []struct {
		name string
		c    storage.UserQueryCriteria
		f    func() *UserFilter
		want []string
	}{
		{"empty", storage.UserQueryCriteria{}, func() *UserFilter { return NewUserFilter() }, []string{"u1", "u2"}},
		{"query", storage.UserQueryCriteria{Query: "alice"}, func() *UserFilter { return NewUserFilter().Query("alice") }, []string{"u1"}},
		{"query case-insensitive", storage.UserQueryCriteria{Query: "BOB@EXAMPLE"}, func() *UserFilter { return NewUserFilter().Query("BOB@EXAMPLE") }, []string{"u2"}},
		{"query no match", storage.UserQueryCriteria{Query: "carol"}, func() *UserFilter { return NewUserFilter().Query("carol") }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := New().BuildUserFilter(tc.c).(*UserFilter)
			if !ok {
				t.Fatalf("BuildUserFilter returned %T, want *UserFilter", got)
			}
			ref := tc.f()
			var ids []string
			for _, u := range users {
				if got.Matches(u) != ref.Matches(u) {
					t.Fatalf("user %s: factory %v, builder chain %v", u.ID(), got.Matches(u), ref.Matches(u))
				}
				if got.Matches(u) {
					ids = append(ids, u.ID())
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("matched %v, want %v", ids, tc.want)
			}
		})
	}
}
