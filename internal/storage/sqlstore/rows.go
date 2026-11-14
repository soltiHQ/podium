package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// querier is what a statement runs on: a pool or a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// reader holds the read methods of the entity stores, and the filter factory.
// q is where its statements run: the primary's pool for the point lookups of the
// store, the pool its routing picked for a listing, a scan or a count, and the
// write transaction for the handle of WithTx, whose every read is the
// transaction's.
type reader struct {
	filters

	d Dialect
	q querier
	// db is the pool q is, when it is one: a scan takes its snapshot on it. It is
	// nil inside a transaction, which is the snapshot already.
	db *sql.DB
}

// writer holds the write methods of the entity stores: its q is a write
// transaction, and its reads are that transaction's. The store runs each write
// method outside WithTx in a transaction of its own, through its gate and fence;
// the handle of WithTx is a writer.
type writer struct{ reader }

// fail returns what a method answers for err, an error of the driver.
func (r *reader) fail(ctx context.Context, err error) error { return classify(ctx, r.d, err) }

// exec runs a statement and returns the number of rows it changed.
func (r *reader) exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := r.q.ExecContext(ctx, r.d.Rebind(query), args...)
	if err != nil {
		return 0, r.fail(ctx, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, r.fail(ctx, err)
	}
	return n, nil
}

// bodies runs a query that selects bodies, and returns them in its order.
func (r *reader) bodies(ctx context.Context, query string, args ...any) ([][]byte, error) {
	rows, err := r.q.QueryContext(ctx, r.d.Rebind(query), args...)
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	defer func() { _ = rows.Close() }()
	var out [][]byte
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, r.fail(ctx, err)
		}
		out = append(out, body)
	}
	if err := rows.Err(); err != nil {
		return nil, r.fail(ctx, err)
	}
	return out, nil
}

// kind ties an entity to its table and its codec.
type kind[T any] struct {
	table  string
	decode func([]byte) (T, error)
}

// byID returns the row of k whose id is id: storage.ErrInvalidArgument for an
// empty ID, storage.ErrNotFound for no row.
func byID[T any](ctx context.Context, r *reader, k kind[T], id string) (T, error) {
	if id == "" {
		var zero T
		return zero, storage.ErrInvalidArgument
	}
	return lookup(ctx, r, k, "id = ?", bin(id))
}

// lookup returns the one row of k that cond holds for, a condition on columns
// that are indexed and not unique, as in the in-memory store: none is
// storage.ErrNotFound, and two are storage.ErrInternal, which names the table
// and no value.
func lookup[T any](ctx context.Context, r *reader, k kind[T], cond string, args ...any) (T, error) {
	var zero T
	found, err := r.bodies(ctx, "SELECT body FROM "+k.table+" WHERE "+cond+" LIMIT 2", args...)
	switch {
	case err != nil:
		return zero, err
	case len(found) == 0:
		return zero, storage.ErrNotFound
	case len(found) > 1:
		return zero, fmt.Errorf("%w: sqlstore: more than one row of %s has the key", storage.ErrInternal, k.table)
	}
	return k.decode(found[0])
}

// deleteByID deletes the row of table whose id is id: storage.ErrInvalidArgument
// for an empty ID, storage.ErrNotFound for no row, from the rows the statement
// deleted, never from a failed statement.
func (w *writer) deleteByID(ctx context.Context, table, id string) error {
	if id == "" {
		return storage.ErrInvalidArgument
	}
	n, err := w.exec(ctx, "DELETE FROM "+table+" WHERE id = ?", bin(id))
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// deleteWhere deletes the rows of table that cond holds for, none included: a
// bulk delete is idempotent.
func (w *writer) deleteWhere(ctx context.Context, table, cond string, args ...any) error {
	_, err := w.exec(ctx, "DELETE FROM "+table+" WHERE "+cond, args...)
	return err
}

// upsertSQL is the statement that writes a row of table by its id, replacing the
// row there: id first, then cols.
func upsertSQL(table string, cols ...string) string {
	set := make([]string, len(cols))
	for i, c := range cols {
		set[i] = c + " = EXCLUDED." + c
	}
	return insertSQL(table, cols...) + " DO UPDATE SET " + strings.Join(set, ", ")
}

// insertSQL is the statement that writes a row of table by its id, id first and
// then cols, unless a row has the id: it changes no row then.
func insertSQL(table string, cols ...string) string {
	names := "id, " + strings.Join(cols, ", ")
	marks := "?" + strings.Repeat(", ?", len(cols))
	return "INSERT INTO " + table + " (" + names + ") VALUES (" + marks + ") ON CONFLICT (id)"
}

// insertIfAbsent is insertSQL as a whole statement: DO NOTHING on an ID that is
// there.
func insertIfAbsent(table string, cols ...string) string {
	return insertSQL(table, cols...) + " DO NOTHING"
}

// columnTime returns t, the what of an entity, as the bigint a time column holds:
// Unix nanoseconds, 0 for the zero time. A time outside the years 1678 to 2262
// is refused with storage.ErrInvalidArgument rather than wrapped; the codecs
// refuse it first, so a row never gets here with one.
func columnTime(what string, t time.Time) (int64, error) {
	if t.IsZero() {
		return 0, nil
	}
	ns := t.UnixNano()
	if !time.Unix(0, ns).Equal(t) {
		return 0, fmt.Errorf("%w: sqlstore: the %s is outside the years 1678 to 2262", storage.ErrInvalidArgument, what)
	}
	return ns, nil
}

// position says where a time a criterion compares lies against the range of a
// time column.
type position int

const (
	inRange position = iota
	belowRange
	aboveRange
)

// timeBound returns t, a bound a criterion compares a time column with, as the
// column holds times. A bound outside the range is not refused: every stored
// time lies inside it, so the criterion's answer is the same for each row, and
// the caller builds it from where t lies, as memory answers.
func timeBound(t time.Time) (int64, position) {
	ns := t.UnixNano()
	switch {
	case time.Unix(0, ns).Equal(t):
		return ns, inRange
	case t.Before(time.Unix(0, 0)):
		return 0, belowRange
	}
	return 0, aboveRange
}

// where is the conditions a statement's WHERE joins with AND, and their
// arguments.
type where struct {
	conds []string
	args  []any
}

// add adds cond, with its arguments, to w.
func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// with returns a copy of w with cond added.
func (w *where) with(cond string, args ...any) *where {
	c := &where{conds: append([]string(nil), w.conds...), args: append([]any(nil), w.args...)}
	c.add(cond, args...)
	return c
}

// sql returns the WHERE clause, empty for no condition.
func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// marks returns n placeholders joined by commas, for an IN list.
func marks(n int) string { return "?" + strings.Repeat(", ?", n-1) }
