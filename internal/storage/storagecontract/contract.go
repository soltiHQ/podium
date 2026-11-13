// Package storagecontract is the storage contract as a test suite: the
// behaviour of storage.Storage that a caller can observe, checked on any
// backend. It describes the in-memory store, the reference backend, so a
// backend that passes it behaves as the in-memory store does. A backend runs it
// from a test of its own, with a constructor that opens an empty store:
//
//	func TestContract(t *testing.T) {
//		storagecontract.Run(t, func(t *testing.T) storage.Storage { return open(t) })
//	}
//
// The suite imports no backend: the foreign filter and the foreign cursor it
// passes are its own, so a store sees them as those of another backend.
package storagecontract

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// storeFunc opens an empty store for a test.
type storeFunc = func(t *testing.T) storage.Storage

// Run checks the storage contract on stores newStore returns, each empty.
//
// The checks are grouped by subtest, so that a backend can run one group at a
// time (go test -run 'TestContract/Agents'). Every check opens its own store and
// none runs in parallel, so newStore may hand out one database, emptied each
// time. Scanner and Counter skip a store that lacks the interface.
func Run(t *testing.T, newStore func(t *testing.T) storage.Storage) {
	groups := []struct {
		name string
		run  func(*testing.T, storeFunc)
	}{
		{"Agents", agents},
		{"AgentCredentials", agentCredentials},
		{"Specs", specs},
		{"Rollouts", rollouts},
		{"Executions", executions},
		{"Users", users},
		{"Credentials", credentials},
		{"Verifiers", verifiers},
		{"Sessions", sessions},
		{"Roles", roles},
		{"Pagination", pagination},
		{"Tx", transactions},
		{"Scanner", scanner},
		{"Counter", counter},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) { g.run(t, newStore) })
	}
}

// epoch is the time every row of the suite is created relative to. It lies in a
// zone other than UTC and between whole microseconds, so that a backend which
// returns a time in another zone passes, the suite comparing times as instants,
// and one which keeps less than nanoseconds fails.
var epoch = time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("contract", 3*60*60))

// minute is m minutes after epoch.
func minute(m int) time.Time { return epoch.Add(time.Duration(m) * time.Minute) }

// errRollback ends a transaction the suite rolls back on purpose.
var errRollback = errors.New("storagecontract: roll the transaction back")

// foreignFilter is a filter no store builds: the suite passes it where a store
// expects a filter of its own. Of the other filters the suite passes, the spec
// is silent on two, and the suite pins what the in-memory store does with them:
// a filter the factory built for another listing is refused, and a nil filter,
// which the suite passes to list or scan every row, lists every row.
type foreignFilter struct{}

// foreignCursor has the shape of an in-memory cursor and the tag of a backend
// that does not exist.
var foreignCursor = base64.RawURLEncoding.EncodeToString([]byte(`{"b":"contract-foreign","v":1,"u":1,"i":"x"}`))

// doneContext returns a context that is already cancelled.
func doneContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// outcome is the error a call returned and the one the contract wants of it:
// nil, or a sentinel that errors.Is finds in it.
type outcome struct {
	call      string
	got, want error
}

// expect fails t for each outcome whose error is not the one wanted.
func expect(t *testing.T, outcomes ...outcome) {
	t.Helper()
	for _, o := range outcomes {
		if !errors.Is(o.got, o.want) {
			t.Errorf("%s: err = %v, want %v", o.call, o.got, o.want)
		}
	}
}

// must stops t on an error the contract does not expect.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// errOf is the error of a call that returns a value too.
func errOf[T any](_ T, err error) error { return err }

// row is what the suite reads of every model: its ID and its creation time.
type row interface {
	ID() string
	CreatedAt() time.Time
}

// idsOf returns the IDs of rows, in order; an empty list for no rows.
func idsOf[T row](rows []T) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID()
	}
	return ids
}

// fields compares the fields of a row read back with those of the row written.
// Every accessor the suite compares returns a nil and an empty slice or map
// alike, so no comparison normalises them; a nil exit code and a zero time stay
// apart from 0 and the Unix epoch.
type fields struct {
	t    *testing.T
	what string // the read, named in a failure
}

// same compares plain values, slices and maps.
func (f fields) same(name string, got, want any) {
	f.t.Helper()
	if !reflect.DeepEqual(got, want) {
		f.t.Errorf("%s: %s = %v, want %v", f.what, name, got, want)
	}
}

// instant compares times as instants: a backend need not keep a time's zone or
// its monotonic reading.
func (f fields) instant(name string, got, want time.Time) {
	f.t.Helper()
	if !got.Equal(want) {
		f.t.Errorf("%s: %s = %v, want %v", f.what, name, got, want)
	}
}

// gotRow compares the row a read returned with the row written.
func gotRow[T row](t *testing.T, what string, got T, err error, want T, equal func(*testing.T, string, T, T)) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	equal(t, what, got, want)
}

// equalRows compares the rows a read returned with the rows written, one by one
// and in order.
func equalRows[T row](t *testing.T, what string, got, want []T, equal func(*testing.T, string, T, T)) {
	t.Helper()
	if !slices.Equal(idsOf(got), idsOf(want)) {
		t.Errorf("%s: rows %v, want %v", what, idsOf(got), idsOf(want))
		return
	}
	for i := range want {
		equal(t, fmt.Sprintf("%s[%d]", what, i), got[i], want[i])
	}
}

// sameIDs fails t unless a read returned the rows want names. The rows of a
// check that uses it are created at one time, so they list in the byte order of
// their IDs.
func sameIDs[T row](t *testing.T, what string, got []T, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	if ids, sorted := idsOf(got), slices.Sorted(slices.Values(want)); !slices.Equal(ids, sorted) {
		t.Errorf("%s: rows %v, want %v", what, ids, sorted)
	}
}

// inRows fails t unless the rows a read returned hold a row equal to want.
func inRows[T row](t *testing.T, what string, rows []T, err error, want T, equal func(*testing.T, string, T, T)) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	for _, r := range rows {
		if r.ID() == want.ID() {
			equal(t, what, r, want)
			return
		}
	}
	t.Errorf("%s: no row %s among %v", what, want.ID(), idsOf(rows))
}

// notInRows fails t when the rows a read returned hold the row id.
func notInRows[T row](t *testing.T, what string, rows []T, err error, id string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	if slices.Contains(idsOf(rows), id) {
		t.Errorf("%s: row %s listed, want it gone", what, id)
	}
}
