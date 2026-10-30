// Package overlay holds a storage transaction in memory, over a base store that
// does not change while it runs. Writes land in the transaction and every read sees
// them: point reads, index lookups, listings and scans. Each write is recorded as
// the SOLTI-CMD operation that makes it, so that a backend which replicates or logs
// its commits runs the callback of WithTx on a Tx, ships Ops once the callback has
// succeeded, and applies them to its state with ApplyOps.
package overlay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/domain"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

var (
	_ storage.Storage                = (*Tx)(nil)
	_ storage.Scanner                = (*Tx)(nil)
	_ storage.ClockedSessionRotation = (*Tx)(nil)
)

// Tx is a transaction over a base store. It reads the base and its own writes, and
// it writes nothing to the base: Ops is what it wrote.
//
// A write is checked as the state machine checks the operation it records when it
// applies it, and a write the state machine would refuse fails here, records
// nothing and leaves the transaction as it was. Deleting a row the transaction does
// not see is such a write: it gives ErrNotFound, as the stores do. A bulk delete
// deletes the rows it matches when it is called, the transaction's own included,
// and not the rows written after it; one that matches none records nothing, since
// the base does not change and the operations apply in order. So a transaction
// that changes nothing is Empty.
//
// The base must not change while the transaction runs: the caller serializes the
// transaction with every writer of the base. A Tx is not safe for concurrent use.
type Tx struct {
	base storage.Storage
	ops  []*raftv1.Op

	agents      writes[*model.Agent]
	agentCreds  writes[*model.AgentCredential]
	users       writes[*model.User]
	roles       writes[*model.Role]
	credentials writes[*model.Credential]
	verifiers   writes[*model.Verifier]
	sessions    writes[*model.Session]
	specs       writes[*model.Spec]
	rollouts    writes[*model.Rollout]
	executions  writes[*model.ExecutionRecord]
}

// New starts a transaction over base.
func New(base storage.Storage) *Tx { return &Tx{base: base} }

// Ops returns the operations the transaction recorded, in the order of the writes
// that recorded them. Applied to the base by ApplyOps, they leave it as the
// transaction reads it. The slice is the transaction's own: callers must not change it.
func (t *Tx) Ops() []*raftv1.Op { return t.ops }

// Empty reports whether the transaction recorded no operation, so that committing
// it would change nothing.
func (t *Tx) Empty() bool { return len(t.ops) == 0 }

func (t *Tx) record(op *raftv1.Op) { t.ops = append(t.ops, op) }

// WithTx runs fn on t: a nested transaction is part of the one around it.
func (t *Tx) WithTx(ctx context.Context, fn func(tx storage.Storage) error) error {
	if fn == nil {
		return storage.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(t)
}

// The filters are the base's: the transaction matches its own rows with them.

func (t *Tx) BuildExecutionFilter(c storage.ExecutionQueryCriteria) storage.ExecutionFilter {
	return t.base.BuildExecutionFilter(c)
}

func (t *Tx) BuildRolloutFilter(c storage.RolloutQueryCriteria) storage.RolloutFilter {
	return t.base.BuildRolloutFilter(c)
}

func (t *Tx) BuildSpecFilter(c storage.SpecQueryCriteria) storage.SpecFilter {
	return t.base.BuildSpecFilter(c)
}

// entity is a stored domain entity. It is a pointer type, so its zero value, nil,
// can stand for a row the transaction deleted.
type entity[T any] interface {
	comparable
	domain.Entity[T]
}

// writes is what a transaction wrote to the rows of one kind, by ID: the row it
// stored, or nil for a row it deleted. Its zero value holds no writes.
type writes[T entity[T]] struct {
	rows map[string]T
}

func (w *writes[T]) put(row T) {
	if w.rows == nil {
		w.rows = make(map[string]T)
	}
	w.rows[row.ID()] = row
}

func (w *writes[T]) drop(id string) {
	var deleted T
	if w.rows == nil {
		w.rows = make(map[string]T)
	}
	w.rows[id] = deleted
}

// written reports whether the transaction wrote or deleted the row id, so that the
// base's row under that ID, if any, is not the one it sees.
func (w *writes[T]) written(id string) bool {
	_, ok := w.rows[id]
	return ok
}

// get returns the row id as the transaction sees it, as a clone the caller owns:
// the row it wrote, ErrNotFound for a row it deleted, and otherwise base's row.
func (w *writes[T]) get(ctx context.Context, id string, base func(context.Context, string) (T, error)) (T, error) {
	if row, ok := w.rows[id]; ok {
		var deleted T
		if row == deleted {
			return deleted, storage.ErrNotFound
		}
		return row.Clone(), nil
	}
	return base(ctx, id)
}

// remove deletes the row id in the transaction. A row the transaction does not see
// stays as it is and gives ErrNotFound.
func (w *writes[T]) remove(ctx context.Context, id string, base func(context.Context, string) (T, error)) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}
	if _, err := w.get(ctx, id, base); err != nil {
		return err
	}
	w.drop(id)
	return nil
}

// removeAll deletes in the transaction every row it sees that match accepts, and
// reports whether there was one. base holds the rows of the base that match
// accepts; a row among them that the transaction wrote is judged by what it wrote
// instead.
func (w *writes[T]) removeAll(base []T, match func(T) bool) bool {
	var deleted T
	removed := false
	for id, row := range w.rows {
		if row != deleted && match(row) {
			w.rows[id] = deleted
			removed = true
		}
	}
	for _, row := range base {
		if !w.written(row.ID()) {
			w.drop(row.ID())
			removed = true
		}
	}
	return removed
}

// own returns clones of the rows the transaction wrote, and did not delete since,
// that match accepts: all of them when match is nil.
func (w *writes[T]) own(match func(T) bool) []T {
	var (
		deleted T
		out     []T
	)
	for _, row := range w.rows {
		if row != deleted && (match == nil || match(row)) {
			out = append(out, row.Clone())
		}
	}
	return out
}

// decoded checks a row as the state machine checks the operation that writes it:
// row and err are what decoding the operation returned, and the store then checks
// the row with storage.ValidateEntity.
func decoded[T entity[T]](row T, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %w", storage.ErrInvalidArgument, err)
	}
	return storage.ValidateEntity(row)
}

// unique returns, as a clone the caller owns, the one row the transaction sees under
// a unique key: among the rows it wrote, the ones match accepts, and the row base
// finds for the key unless the transaction wrote that row. It is ErrNotFound with
// none and ErrInternal with more than one; key names the key in that error. The
// base finds its row with its own lookup, so a base that already holds two rows
// under the key fails the lookup, whatever the transaction wrote.
func unique[T entity[T]](w *writes[T], match func(T) bool, base func() (T, error), key string) (T, error) {
	var zero T
	found := w.own(match)
	switch row, err := base(); {
	case err == nil:
		if !w.written(row.ID()) {
			found = append(found, row)
		}
	case !errors.Is(err, storage.ErrNotFound):
		return zero, err
	}
	switch len(found) {
	case 0:
		return zero, storage.ErrNotFound
	case 1:
		return found[0], nil
	default:
		return zero, fmt.Errorf("%w: non-unique %s", storage.ErrInternal, key)
	}
}

// scan is a scan of the transaction: the rows all returns, a scan of the base with
// filter, as the transaction sees them. The base checks filter, so all must run
// before the transaction matches its own rows with it.
func scan[T entity[T]](w *writes[T], filter any, all func() ([]T, error)) ([]T, error) {
	rows, err := all()
	if err != nil {
		return nil, err
	}
	match, err := matcher[T](filter)
	if err != nil {
		return nil, err
	}
	return merged(rows, w, match), nil
}

// matcher returns the predicate of a filter the base accepted, the method the
// filters of the in-memory store match a row with. A nil filter matches every row.
func matcher[T any](filter any) (func(T) bool, error) {
	if filter == nil {
		return nil, nil
	}
	f, ok := filter.(interface{ Matches(T) bool })
	if !ok {
		return nil, storage.ErrInvalidArgument
	}
	return f.Matches, nil
}

// merged returns base, rows of the base in the listing order, as the transaction
// sees them: without the rows it wrote or deleted, and with its own rows that match
// accepts, each in its place in the listing order. A nil match accepts every row.
// It leaves base as it is: when the transaction wrote no row of the kind, it
// returns base itself, and otherwise a new slice. It is nil when no row is left, as
// a scan is.
func merged[T entity[T]](base []T, w *writes[T], match func(T) bool) []T {
	if len(w.rows) == 0 {
		if len(base) == 0 {
			return nil
		}
		return base
	}
	own := w.own(match)
	sort.Slice(own, func(i, j int) bool { return listLess(own[i], own[j]) })
	out := make([]T, 0, len(base)+len(own))
	for _, row := range base {
		if w.written(row.ID()) {
			continue
		}
		for len(own) > 0 && listLess(own[0], row) {
			out, own = append(out, own[0]), own[1:]
		}
		out = append(out, row)
	}
	out = append(out, own...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// listLess is the listing order of storage.ListOptions: CreatedAt DESC, then ID
// ASC. CreatedAt is compared in UnixNano, the clock a cursor keeps.
func listLess[T entity[T]](a, b T) bool {
	ta, tb := a.CreatedAt().UnixNano(), b.CreatedAt().UnixNano()
	if ta != tb {
		return ta > tb
	}
	return a.ID() < b.ID()
}

// list is a listing of the transaction: the page opts asks for of the rows source
// returns, which are in the listing order. A page holds clones of its rows only.
func list[T entity[T]](opts storage.ListOptions, source func() ([]T, error)) (*storage.ListResult[T], error) {
	after, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := source()
	if err != nil {
		return nil, err
	}
	start := 0
	if after != nil {
		start = sort.Search(len(rows), func(i int) bool { return after.precedes(rows[i].CreatedAt().UnixNano(), rows[i].ID()) })
	}
	end := min(start+storage.NormalizeLimit(opts.Limit), len(rows))
	if start >= end {
		return &storage.ListResult[T]{Items: []T{}}, nil
	}
	page := &storage.ListResult[T]{Items: slices.Clone(rows[start:end])}
	if end < len(rows) {
		last := rows[end-1]
		if page.NextCursor, err = encodeCursor(cursor{CreatedAt: last.CreatedAt().UnixNano(), ID: last.ID()}); err != nil {
			return nil, err
		}
	}
	return page, nil
}

const (
	cursorBackend = "overlay"
	cursorVersion = 1
)

// cursor is the position of the last row of a page in the listing order: the next
// page starts after it, whatever was written in between. Callers see an opaque
// base64 token, which names its backend and version, as an in-memory cursor does.
type cursor struct {
	Backend   string `json:"b"`
	Version   int    `json:"v"`
	CreatedAt int64  `json:"u"`
	ID        string `json:"i"`
}

// precedes reports whether the cursor comes before the row created at createdAt,
// in UnixNano, with the given ID.
func (c *cursor) precedes(createdAt int64, id string) bool {
	if createdAt != c.CreatedAt {
		return createdAt < c.CreatedAt
	}
	return id > c.ID
}

func encodeCursor(c cursor) (string, error) {
	c.Backend, c.Version = cursorBackend, cursorVersion
	b, err := json.Marshal(c)
	if err != nil {
		return "", storage.ErrInternal
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor returns the cursor s encodes, and nil for the empty s that starts a
// listing. A cursor this backend did not make is ErrInvalidArgument.
func decodeCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, storage.ErrInvalidArgument
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, storage.ErrInvalidArgument
	}
	if c.Backend != cursorBackend || c.Version != cursorVersion || c.ID == "" {
		return nil, storage.ErrInvalidArgument
	}
	return &c, nil
}
