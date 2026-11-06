package user

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/domain/wire"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// etagUser builds a user with every stored field set, both stamps at instant.
func etagUser(t *testing.T, id string, instant time.Time) *model.User {
	t.Helper()
	u, err := model.NewUser(id, "subject")
	if err != nil {
		t.Fatal(err)
	}
	u.SetEmail("user@example.com")
	u.SetName("User")
	u.SetRoleIDs([]string{"role-a", "role-b"})
	u.SetPermissions([]string{string(enum.UsersGet)})
	u.SetCreatedAt(instant)
	u.SetUpdatedAt(instant)
	return u
}

// Each stored field moves the ETag on its own, including the stamps. The edits
// of the fields put the update time back, so the field alone is what moves it.
func TestETagCoversEveryStoredField(t *testing.T) {
	instant := time.Date(2026, 11, 6, 9, 0, 0, 123, time.UTC)
	want := ETag(etagUser(t, "id", instant))
	if want == "" {
		t.Fatal("ETag is empty")
	}
	if got := ETag(etagUser(t, "id", instant)); got != want {
		t.Fatalf("the same user has the ETags %s and %s", want, got)
	}
	if got := ETag(etagUser(t, "id", instant).Clone()); got != want {
		t.Fatalf("a clone has the ETag %s, want %s", got, want)
	}

	keepStamp := func(edit func(*model.User)) func(*model.User) {
		return func(u *model.User) {
			edit(u)
			u.SetUpdatedAt(instant)
		}
	}
	seen := map[string]string{want: "unchanged"}
	for _, tc := range []struct {
		name string
		edit func(*model.User)
	}{
		{"subject", keepStamp(func(u *model.User) { u.SetSubject("other") })},
		{"email", keepStamp(func(u *model.User) { u.SetEmail("other@example.com") })},
		{"name", keepStamp(func(u *model.User) { u.SetName("Other") })},
		{"disabled", keepStamp((*model.User).Disable)},
		{"role added", keepStamp(func(u *model.User) { _ = u.RoleAdd("role-c") })},
		{"role removed", keepStamp(func(u *model.User) { u.RoleDelete("role-b") })},
		{"role replaced", keepStamp(func(u *model.User) { u.SetRoleIDs([]string{"role-a", "role-c"}) })},
		{"permission added", keepStamp(func(u *model.User) { _ = u.PermissionAdd(enum.UsersEdit) })},
		{"permission removed", keepStamp(func(u *model.User) { u.PermissionDelete(enum.UsersGet) })},
		{"permission replaced", keepStamp(func(u *model.User) { u.SetPermissions([]string{string(enum.UsersEdit)}) })},
		{"creation time", func(u *model.User) { u.SetCreatedAt(instant.Add(time.Nanosecond)) }},
		{"update time", func(u *model.User) { u.SetUpdatedAt(instant.Add(time.Nanosecond)) }},
	} {
		u := etagUser(t, "id", instant)
		tc.edit(u)
		got := ETag(u)
		if other, ok := seen[got]; ok {
			t.Errorf("%s gives the ETag of %s: %s", tc.name, other, got)
		}
		seen[got] = tc.name
	}

	if got := ETag(etagUser(t, "other-id", instant)); got == want {
		t.Errorf("another ID has the same ETag %s", got)
	}
}

// Moving a character from one field to the next is another user.
func TestETagSeparatesFields(t *testing.T) {
	instant := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	mk := func(subject, name string, roles, perms []string) string {
		u := etagUser(t, "id", instant)
		u.SetSubject(subject)
		u.SetName(name)
		u.SetRoleIDs(roles)
		u.SetPermissions(perms)
		u.SetUpdatedAt(instant)
		return ETag(u)
	}
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{"subject and name", mk("ab", "c", nil, nil), mk("a", "bc", nil, nil)},
		{"one role or two", mk("s", "n", []string{"a", "b"}, nil), mk("s", "n", []string{"ab"}, nil)},
		{"roles or permissions", mk("s", "n", []string{"x"}, nil), mk("s", "n", nil, []string{"x"})},
	} {
		if tc.a == tc.b {
			t.Errorf("%s: two different users share the ETag %s", tc.name, tc.a)
		}
	}
}

// A stored user is read back from the log or a snapshot as the wire form builds
// it, with its stamps in nanoseconds: the ETag read before and after must agree,
// whichever node serves the read.
func TestETagSurvivesTheWireRoundTrip(t *testing.T) {
	now := time.Now()
	u := etagUser(t, "id", now)
	u.SetCreatedAt(now.Add(-time.Hour))
	back, err := wire.UserFromProto(wire.UserToProto(u))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ETag(back), ETag(u); got != want {
		t.Fatalf("the ETag after the wire round trip is %s, want %s", got, want)
	}
}

// Every way the service writes a user moves the ETag, and so does a write that
// restores the update time it replaces. A write that stores what is already
// stored does not, nor does a change to a role the user holds.
func TestETagMovesOnEveryWritePath(t *testing.T) {
	svc, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}})
	ctx := context.Background()
	admin := actor("admin", enum.All...)
	tag := func() string { return ETag(stored(t, st, "target")) }

	moved := func(name string, write func() error) {
		t.Helper()
		before := tag()
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := tag(); after == before {
			t.Fatalf("%s left the ETag as it was: %s", name, after)
		}
	}
	kept := func(name string, write func() error) {
		t.Helper()
		before := tag()
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := tag(); after != before {
			t.Fatalf("%s moved the ETag from %s to %s", name, before, after)
		}
	}
	update := func(patch UpdatePatch) func() error {
		return func() error { _, err := svc.Update(ctx, admin, "target", patch); return err }
	}
	disable := func(disabled bool) func() error {
		return func() error { _, err := svc.SetDisabled(ctx, admin, "target", disabled); return err }
	}

	moved("Update name", update(UpdatePatch{Name: ptr("Renamed")}))
	moved("Update roles", update(UpdatePatch{RoleIDs: ptr([]string{enum.RoleUserEditorID})}))
	moved("Update permissions", update(UpdatePatch{Permissions: ptr([]string{string(enum.AgentsGet)})}))
	moved("SetDisabled(true)", disable(true))
	moved("SetDisabled(false)", disable(false))
	moved("Upsert", func() error {
		u := stored(t, st, "target")
		u.SetEmail("target@example.com")
		return svc.Upsert(ctx, u)
	})
	moved("a write that keeps the update time", func() error {
		u := stored(t, st, "target")
		stamp := u.UpdatedAt()
		u.SetName("Replayed")
		u.SetUpdatedAt(stamp)
		return st.UpsertUser(ctx, u)
	})

	kept("a change to the permissions of a role the user holds", func() error {
		r, err := st.GetRole(ctx, enum.RoleUserEditorID)
		if err != nil {
			return err
		}
		if err = r.PermissionAdd(enum.AgentsEdit); err != nil {
			return err
		}
		return st.UpsertRole(ctx, r)
	})
	kept("Update with what is stored", update(UpdatePatch{Name: ptr("Replayed")}))
	kept("SetDisabled to the stored state", disable(false))
	kept("Update with an empty patch", update(UpdatePatch{}))
}

func TestUpdateIfMatch(t *testing.T) {
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)
	for _, tc := range []struct {
		name    string
		ifMatch func(current string) []string
		stale   bool
	}{
		{"nil updates the stored user as it is", func(string) []string { return nil }, false},
		{"the stored tag", func(c string) []string { return []string{c} }, false},
		{"the stored tag among stale ones", func(c string) []string { return []string{"stale", c, "other"} }, false},
		{"a stale tag", func(string) []string { return []string{"stale"} }, true},
		{"an empty list names no tag", func(string) []string { return []string{} }, true},
		{"an empty tag", func(string) []string { return []string{""} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}})
			before := stored(t, st, "target")
			patch := UpdatePatch{Name: ptr("Renamed"), RoleIDs: ptr([]string{enum.RoleUserEditorID}), IfMatch: tc.ifMatch(ETag(before))}

			got, err := svc.Update(ctx, editor, "target", patch)
			after := stored(t, st, "target")
			if tc.stale {
				if !errors.Is(err, ErrStale) {
					t.Fatalf("Update = %v, want ErrStale", err)
				}
				if got != nil {
					t.Fatal("Update returned a user with ErrStale")
				}
				if !profileOf(after).same(profileOf(before)) || ETag(after) != ETag(before) || !after.UpdatedAt().Equal(before.UpdatedAt()) {
					t.Fatalf("the refused update changed the user: %+v", profileOf(after))
				}
				return
			}
			if err != nil {
				t.Fatalf("Update = %v", err)
			}
			want := profile{subject: "target", name: "Renamed", roles: []string{enum.RoleUserEditorID}}
			if !profileOf(after).same(want) || !profileOf(got).same(want) {
				t.Fatalf("stored %+v, returned %+v, want %+v", profileOf(after), profileOf(got), want)
			}
		})
	}
}

// The tag is checked after the ceiling, so a caller who may not manage the user
// learns nothing from the answer, and it is checked against a user that exists.
func TestUpdateIfMatchComesAfterTheCeilingAndTheLookup(t *testing.T) {
	svc, st := fixture(t, map[string][]string{"user-admin": {enum.RoleAdminID}})
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)
	current := ETag(stored(t, st, "user-admin"))

	for _, ifMatch := range [][]string{{"stale"}, {current}} {
		patch := UpdatePatch{Name: ptr("Renamed"), IfMatch: ifMatch}
		if _, err := svc.Update(ctx, editor, "user-admin", patch); !errors.Is(err, ErrPermissionCeiling) {
			t.Fatalf("Update of an Admin with If-Match %q = %v, want ErrPermissionCeiling", ifMatch, err)
		}
	}
	if _, err := svc.Update(ctx, editor, "missing", UpdatePatch{Name: ptr("Missing"), IfMatch: []string{"stale"}}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Update of an unknown user = %v, want ErrNotFound", err)
	}
}

// The tags are compared in the transaction that applies the patch: a change that
// commits after the caller read its tag, and before the transaction starts,
// leaves the tag stale and the patch unapplied.
func TestUpdateIfMatchIsCheckedInsideTheTransaction(t *testing.T) {
	_, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}})
	tag := ETag(stored(t, st, "target"))
	svc := racing(t, st, "target", func(u *model.User) { u.SetName("Changed meanwhile") })

	patch := UpdatePatch{RoleIDs: ptr([]string{enum.RoleUserEditorID}), IfMatch: []string{tag}}
	if _, err := svc.Update(context.Background(), actor("editor", userEditorPerms...), "target", patch); !errors.Is(err, ErrStale) {
		t.Fatalf("Update over a change that committed meanwhile = %v, want ErrStale", err)
	}
	want := profile{subject: "target", name: "Changed meanwhile", roles: []string{enum.RoleUserReaderID}}
	if got := profileOf(stored(t, st, "target")); !got.same(want) {
		t.Fatalf("stored %+v, want only the concurrent change: %+v", got, want)
	}
}

// Writers that hold the same tag race for one transaction each: the first one
// stores its change and every later one finds the tag replaced.
func TestUpdateIfMatchAdmitsOneOfManyWriters(t *testing.T) {
	svc, st := fixture(t, map[string][]string{"target": {enum.RoleUserReaderID}})
	ctx := context.Background()
	editor := actor("editor", userEditorPerms...)
	tag := ETag(stored(t, st, "target"))

	const writers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, writers)
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.Update(ctx, editor, "target", UpdatePatch{Name: ptr(fmt.Sprintf("writer-%d", i)), IfMatch: []string{tag}})
		}()
	}
	close(start)
	wg.Wait()

	var winners []int
	for i, err := range errs {
		switch {
		case err == nil:
			winners = append(winners, i)
		case !errors.Is(err, ErrStale):
			t.Errorf("writer %d: %v, want nil or ErrStale", i, err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d of %d writers holding one tag stored their change (%v), want 1", len(winners), writers, winners)
	}
	if got, want := stored(t, st, "target").Name(), fmt.Sprintf("writer-%d", winners[0]); got != want {
		t.Fatalf("stored name %q, want the winner's %q", got, want)
	}
}
