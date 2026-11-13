package storagecontract

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// pagination checks every paginated listing, one subtest each: the listing
// order, the cursor, the limits and the empty page.
func pagination(t *testing.T, newStore storeFunc) {
	for _, l := range listings() {
		t.Run(l.name, func(t *testing.T) {
			t.Run("listing order", func(t *testing.T) { listingOrder(t, newStore(t), l) })
			t.Run("cursor drains every row once", func(t *testing.T) { drainsOnce(t, newStore(t), l) })
			t.Run("limits", func(t *testing.T) { limits(t, newStore(t), l) })
			t.Run("malformed or foreign cursor", func(t *testing.T) { badCursors(t, newStore(t), l) })
			t.Run("empty page", func(t *testing.T) { emptyPages(t, newStore(t), l) })
			t.Run("pages hold their place", func(t *testing.T) { holdsPlace(t, newStore(t), l) })
		})
	}
}

// listing is a paginated List method of the store, and the rows it lists.
type listing struct {
	name string
	// put writes the row of key, created at created, and returns its ID. Rows of
	// keys in order have IDs in order, but for execution records, whose IDs are
	// digests of their run.
	put func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error)
	del func(ctx context.Context, s storage.Storage, id string) error
	// page lists one page: the IDs of its rows, nil when its Items is nil, and
	// its cursor.
	page func(ctx context.Context, s storage.Storage, opts storage.ListOptions) ([]string, string, error)
}

func listings() []listing {
	return []listing{
		{
			name: "ListAgents",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				a := fullAgent(t, "agent-"+key, 0, created)
				return a.ID(), s.UpsertAgent(t.Context(), a)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteAgent(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListAgents(ctx, nil, o))
			},
		},
		{
			name: "ListAgentCredentials",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				c := storedAgentCredential(t, "agent-"+key, 0, created)
				return c.ID(), s.UpsertAgentCredential(t.Context(), c)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteAgentCredential(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListAgentCredentials(ctx, o))
			},
		},
		{
			name: "ListUsers",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				u := fullUser(t, "user-"+key, "sub-"+key, 0, created)
				return u.ID(), s.UpsertUser(t.Context(), u)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteUser(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListUsers(ctx, nil, o))
			},
		},
		{
			name: "ListRoles",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				r := fullRole(t, "role-"+key, "role "+key, 0, created)
				return r.ID(), s.UpsertRole(t.Context(), r)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteRole(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListRoles(ctx, nil, o))
			},
		},
		{
			name: "ListSpecs",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				sp := fullSpec(t, "spec-"+key, 0, created)
				return sp.ID(), s.UpsertSpec(t.Context(), sp)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteSpec(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListSpecs(ctx, nil, o))
			},
		},
		{
			name: "ListRollouts",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				r := fullRollout(t, "spec-"+key, "agent-a", 0, created)
				return r.ID(), s.UpsertRollout(t.Context(), r)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteRollout(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListRollouts(ctx, nil, o))
			},
		},
		{
			name: "ListExecutions",
			put: func(t *testing.T, s storage.Storage, key string, created time.Time) (string, error) {
				e := fullExecution(t, "agent-a", "uid-"+key, 0, created)
				return e.ID(), s.UpsertExecution(t.Context(), e)
			},
			del: func(ctx context.Context, s storage.Storage, id string) error { return s.DeleteExecution(ctx, id) },
			page: func(ctx context.Context, s storage.Storage, o storage.ListOptions) ([]string, string, error) {
				return pageOf(s.ListExecutions(ctx, nil, o))
			},
		},
	}
}

// pageOf returns the IDs of a page's rows, nil when its Items is nil, and its
// cursor.
func pageOf[T row](page *storage.ListResult[T], err error) ([]string, string, error) {
	switch {
	case err != nil:
		return nil, "", err
	case page == nil:
		return nil, "", errors.New("a nil page")
	case page.Items == nil:
		return nil, page.NextCursor, nil
	}
	return idsOf(page.Items), page.NextCursor, nil
}

// keyed is a row to write: its key and its creation time.
type keyed struct {
	key     string
	created time.Time
}

// written is a row written: its ID and its creation time.
type written struct {
	id      string
	created time.Time
}

// putRows writes rows into s through l.
func putRows(t *testing.T, s storage.Storage, l listing, rows ...keyed) []written {
	t.Helper()
	out := make([]written, len(rows))
	for i, r := range rows {
		id, err := l.put(t, s, r.key, r.created)
		must(t, err)
		out[i] = written{id, r.created}
	}
	return out
}

// listed returns the IDs of rows in the order of every listing: newest first,
// and rows created at one time in ID order.
func listed(rows []written) []string {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b written) int {
		if c := b.created.Compare(a.created); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.id
	}
	return ids
}

// orderRows are rows whose listing order, g, b, e, c, f, a, d, is neither the
// order of their IDs nor that of their writes. Two rows share each minute, and
// in two of the pairs the row first by ID is written second, so that a store
// which breaks a tie in the order of the writes fails. g is a nanosecond after
// b and e, so that a store which keeps less than nanoseconds ties them.
var orderRows = []keyed{
	{"e", minute(3)},
	{"d", minute(1)},
	{"c", minute(2)},
	{"b", minute(3)},
	{"a", minute(1)},
	{"f", minute(2)},
	{"g", minute(3).Add(time.Nanosecond)},
}

// drain lists every row from cursor on, limit rows a page, and returns their
// IDs. It fails t when a page holds more rows than the limit, a page before the
// last fewer, the last none after others, or the listing does not end.
func drain(t *testing.T, s storage.Storage, l listing, cursor string, limit int) []string {
	t.Helper()
	var ids []string
	for range 1000 {
		page, next, err := l.page(t.Context(), s, storage.ListOptions{Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("%s with limit %d: %v", l.name, limit, err)
		}
		if limit > 0 && len(page) > limit {
			t.Errorf("%s with limit %d: a page of %d rows", l.name, limit, len(page))
		}
		if next == "" {
			if len(page) == 0 && len(ids) > 0 {
				t.Errorf("%s with limit %d: an empty last page; the page before it had no cursor to give", l.name, limit)
			}
			return append(ids, page...)
		}
		if limit > 0 && len(page) < limit {
			t.Errorf("%s with limit %d: a page of %d rows before the last", l.name, limit, len(page))
		}
		ids = append(ids, page...)
		cursor = next
	}
	t.Fatalf("%s with limit %d: the listing did not end in 1000 pages", l.name, limit)
	return nil
}

// listingOrder checks the order (CreatedAt DESC, ID ASC), on one page and
// across pages whose cursors fall between tied rows.
func listingOrder(t *testing.T, s storage.Storage, l listing) {
	want := listed(putRows(t, s, l, orderRows...))
	for _, limit := range []int{0, 2} {
		if got := drain(t, s, l, "", limit); !slices.Equal(got, want) {
			t.Errorf("%s with limit %d: %v, want %v", l.name, limit, got, want)
		}
	}
}

// drainsOnce checks that the cursor drains every row once at every page size:
// every page full but the last, and the last with no cursor, even when it is full.
func drainsOnce(t *testing.T, s storage.Storage, l listing) {
	want := listed(putRows(t, s, l, orderRows...))
	for limit := 1; limit <= len(want)+1; limit++ {
		if got := drain(t, s, l, "", limit); !slices.Equal(got, want) {
			t.Errorf("%s with limit %d: %v, want %v", l.name, limit, got, want)
		}
	}
}

// limits checks the page size: 100 by default, 500 at most.
func limits(t *testing.T, s storage.Storage, l listing) {
	ctx := t.Context()
	// One row more than the largest page holds, written in one transaction.
	var rows []written
	must(t, s.WithTx(ctx, func(tx storage.Storage) error {
		for i := range storage.MaxListLimit + 1 {
			id, err := l.put(t, tx, fmt.Sprintf("%03d", i), minute(i))
			if err != nil {
				return err
			}
			rows = append(rows, written{id, minute(i)})
		}
		return nil
	}))
	order := listed(rows)
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"zero, the default", 0, storage.DefaultListLimit},
		// The spec is silent on a limit below one and on one above 500; the
		// in-memory store gives the default for the one and clamps the other,
		// as ListOptions documents.
		{"negative, the default", -1, storage.DefaultListLimit},
		{"one", 1, 1},
		{"the largest", storage.MaxListLimit, storage.MaxListLimit},
		{"above the largest, clamped", storage.MaxListLimit + 1, storage.MaxListLimit},
	} {
		page, next, err := l.page(ctx, s, storage.ListOptions{Limit: tc.limit})
		if err != nil {
			t.Errorf("%s with a limit %s: %v", l.name, tc.name, err)
			continue
		}
		if !slices.Equal(page, order[:tc.want]) {
			t.Errorf("%s with a limit %s: %d rows, want the first %d", l.name, tc.name, len(page), tc.want)
		}
		if next == "" {
			t.Errorf("%s with a limit %s: no cursor, want one to the rows left", l.name, tc.name)
		}
		if tc.limit == storage.MaxListLimit {
			rest, last, err := l.page(ctx, s, storage.ListOptions{Limit: tc.limit, Cursor: next})
			if err != nil || !slices.Equal(rest, order[storage.MaxListLimit:]) || last != "" {
				t.Errorf("%s, the page after the largest: rows %v, cursor %q, err %v; want the last row and no cursor", l.name, rest, last, err)
			}
		}
	}
}

// badCursors checks that a cursor which is not the store's is refused.
func badCursors(t *testing.T, s storage.Storage, l listing) {
	// A row, so that a store which ignored the cursor would have a page to give.
	putRows(t, s, l, keyed{"a", minute(0)})
	for _, tc := range []struct{ name, cursor string }{
		{"not base64url", "%%% not a cursor %%%"},
		{"base64url of what is not JSON", base64.RawURLEncoding.EncodeToString([]byte("not JSON"))},
		{"tagged for another backend", foreignCursor},
	} {
		_, _, err := l.page(t.Context(), s, storage.ListOptions{Cursor: tc.cursor})
		expect(t, outcome{l.name + " with a cursor " + tc.name, err, storage.ErrInvalidArgument})
	}
}

// emptyPages checks that a page with no row is an empty list, not nil, and has
// no cursor: that of an empty store, and that past the last row.
func emptyPages(t *testing.T, s storage.Storage, l listing) {
	ctx := t.Context()
	page, next, err := l.page(ctx, s, storage.ListOptions{})
	must(t, err)
	emptyPage(t, l.name+" of an empty store", page, next)

	// The rows after a cursor deleted, it points past the last row.
	order := listed(putRows(t, s, l, orderRows[:3]...))
	_, next, err = l.page(ctx, s, storage.ListOptions{Limit: 2})
	must(t, err)
	must(t, l.del(ctx, s, order[2]))
	page, next, err = l.page(ctx, s, storage.ListOptions{Limit: 2, Cursor: next})
	must(t, err)
	emptyPage(t, l.name+" past the last row", page, next)
}

func emptyPage(t *testing.T, what string, page []string, next string) {
	t.Helper()
	switch {
	case page == nil:
		t.Errorf("%s: nil Items, want an empty list", what)
	case len(page) > 0:
		t.Errorf("%s: rows %v, want none", what, page)
	}
	if next != "" {
		t.Errorf("%s: cursor %q, want none", what, next)
	}
}

// holdsPlace checks that a cursor is a place in the listing order, not an
// offset: rows written or deleted between two pages move no other row, the
// cursor's own row included.
func holdsPlace(t *testing.T, s storage.Storage, l listing) {
	ctx := t.Context()
	rows := putRows(t, s, l, keyed{"a", minute(6)}, keyed{"b", minute(5)}, keyed{"c", minute(5)}, keyed{"d", minute(5)}, keyed{"e", minute(4)}, keyed{"f", minute(3)})
	first, next, err := l.page(ctx, s, storage.ListOptions{Limit: 2})
	must(t, err)
	if len(first) != 2 || next == "" {
		t.Fatalf("%s: a first page of %v and cursor %q, want two rows and a cursor", l.name, first, next)
	}

	// The cursor stands at a row tied with two others. Between the pages that row
	// is deleted, and rows are written before it, tied with it and after it.
	at := first[1]
	i := slices.IndexFunc(rows, func(r written) bool { return r.id == at })
	cursor := rows[i]
	must(t, l.del(ctx, s, at))
	rows = slices.Delete(rows, i, i+1)
	rows = append(rows, putRows(t, s, l, keyed{"g", minute(9)}, keyed{"h", minute(5)}, keyed{"i", minute(1)})...)

	var after []written
	for _, r := range rows {
		if r.created.Before(cursor.created) || (r.created.Equal(cursor.created) && r.id > cursor.id) {
			after = append(after, r)
		}
	}
	if got, want := drain(t, s, l, next, 2), listed(after); !slices.Equal(got, want) {
		t.Errorf("%s from a cursor at %s: %v, want %v", l.name, at, got, want)
	}
}
