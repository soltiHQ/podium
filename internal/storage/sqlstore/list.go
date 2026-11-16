package sqlstore

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/storage"
)

// pageQuery describes a listing of one table: the exact criteria SQL applies,
// and the predicate Go applies to the rows SQL returns.
type pageQuery[T any] struct {
	kind[T]
	// where holds the exact criteria; SQL applies nothing else but the keyset
	// and the order.
	where *where
	// match is the predicate Go applies to a decoded row; nil: every row.
	match func(T) bool
}

// rawRow is a row as a listing reads it, before its body is decoded.
type rawRow struct {
	id        []byte
	createdAt int64
	body      []byte
}

// listPage returns the page of q that opts asks for, in the listing order
// (created_at DESC, id ASC), by keyset: the rows after the place the cursor
// names. A page of an unfiltered listing is one statement of limit+1 rows. A
// listing with a Go predicate reads keyset batches until it holds limit+1
// matches or the rows end, so that no matching row is skipped or returned twice
// across pages, and a page is full unless the rows are over. The cursor names
// the last row returned, and is empty on the last page.
func listPage[T any](ctx context.Context, r *reader, q pageQuery[T], opts storage.ListOptions) (*storage.ListResult[T], error) {
	after, err := decodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	limit := storage.NormalizeLimit(opts.Limit)
	batch := limit + 1
	if q.match != nil {
		// Matches are sparse as far as the filter is, and a decoded row costs: read
		// a few pages' worth at a time, bounded.
		batch = min(max(2*(limit+1), 64), 1000)
	}

	var (
		items []T
		rows  []rawRow // the rows items were decoded from, in step with items
		more  bool
	)
read:
	for {
		batchRows, err := keyset(ctx, r, q.table, q.where, after, batch)
		if err != nil {
			return nil, err
		}
		for _, row := range batchRows {
			if q.match == nil && len(items) == limit {
				// A row after the page: only that there is one matters.
				more = true
				break read
			}
			item, err := q.decode(row.body)
			if err != nil {
				return nil, err
			}
			if q.match != nil && !q.match(item) {
				continue
			}
			if len(items) == limit {
				more = true
				break read
			}
			items = append(items, item)
			rows = append(rows, row)
		}
		if len(batchRows) < batch {
			break
		}
		last := batchRows[len(batchRows)-1]
		after = cursor{CreatedAt: last.createdAt, ID: last.id}
	}

	if items == nil {
		items = []T{}
	}
	page := &storage.ListResult[T]{Items: items}
	if more {
		last := rows[len(rows)-1]
		page.NextCursor = encodeCursor(last.createdAt, last.id)
	}
	return page, nil
}

// keyset reads up to n rows of table after the place in the listing order that
// after names, the start for none, that w holds for.
func keyset(ctx context.Context, r *reader, table string, w *where, after cursor, n int) ([]rawRow, error) {
	if len(after.ID) > 0 {
		// Created before it, or at its time and later by ID.
		w = w.with("(created_at < ? OR (created_at = ? AND id > ?))", after.CreatedAt, after.CreatedAt, after.ID)
	}
	args := append(append([]any(nil), w.args...), n)
	rows, err := r.q.QueryContext(ctx,
		r.d.Rebind("SELECT id, created_at, body FROM "+table+w.sql()+" ORDER BY created_at DESC, id ASC LIMIT ?"), args...)
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	defer func() { _ = rows.Close() }()
	var out []rawRow
	for rows.Next() {
		var row rawRow
		if err := rows.Scan(&row.id, &row.createdAt, &row.body); err != nil {
			return nil, r.fail(ctx, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, r.fail(ctx, err)
	}
	return out, nil
}
