package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// The lease's statements. Each runs on the lease's pool, on one connection,
// under the statement_timeout onLease sets first; the takeover's two run in a
// transaction of their own, lockForTakeover first. now() is the database's
// clock: in a statement of its own, the time the statement starts; in a
// transaction, the time the transaction started, where statement_timestamp()
// is the statement's own start. A lease of $3 is in microseconds.
//
// The fence reads the row FOR KEY SHARE (fenceRead), a lock that holds it
// against FOR UPDATE alone. renewLease and releaseLease change expires_at
// alone, no-key updates, which never wait for a fence. A statement that changes
// epoch or holder must run after lockForTakeover, in the same transaction:
// Postgres then records its update as a key update, which a fence waits for,
// then re-reads the row after. An UPDATE of epoch or holder without that lock
// is a no-key update: a fence neither waits for it nor re-reads the row after
// it.
const (
	// lockForTakeover locks the row FOR UPDATE, the first statement of a
	// takeover: it waits for the write transactions in flight, whose fences hold
	// the row FOR KEY SHARE, and makes the takeover's update one that a fence
	// waits for. A missing row locks nothing: takeLease inserts it.
	lockForTakeover = `SELECT 1 FROM podium_leader WHERE id = 1 FOR UPDATE`

	// takeLease takes the lease when the row is free or expired, raising its
	// epoch, also when this replica held it last, and returns the epoch taken. A
	// row that another replica holds, unexpired, returns no row: the conflict
	// locks it, and the WHERE leaves it, a lease renewed while lockForTakeover
	// waited included. It runs after lockForTakeover, in its transaction:
	// statement_timestamp() is the upsert's own start, after the lock's wait,
	// where now() would be the transaction's, before it.
	takeLease = `INSERT INTO podium_leader (id, epoch, holder, address, expires_at)
VALUES (1, 1, $1, $2, statement_timestamp() + $3::bigint * interval '1 microsecond')
ON CONFLICT (id) DO UPDATE
   SET epoch = podium_leader.epoch + 1,
       holder = excluded.holder,
       address = excluded.address,
       expires_at = excluded.expires_at
 WHERE podium_leader.expires_at <= statement_timestamp()
RETURNING epoch`

	// renewLease moves the expiry of the lease this replica holds at epoch $1. It
	// changes no row once the row moved: another replica took it, or it is gone.
	renewLease = `UPDATE podium_leader SET expires_at = now() + $3::bigint * interval '1 microsecond'
 WHERE id = 1 AND epoch = $1 AND holder = $2`

	// releaseLease ends now the lease this replica holds at epoch $1, epoch and
	// holder kept, so that its writes stay fenced until another replica takes
	// it.
	releaseLease = `UPDATE podium_leader SET expires_at = now() WHERE id = 1 AND epoch = $1 AND holder = $2`

	// readHolder reads the lease's holder, and the time left until its expiry, in
	// microseconds, negative once expired.
	readHolder = `SELECT holder, address, floor(extract(epoch FROM expires_at - now()) * 1000000)::bigint
  FROM podium_leader WHERE id = 1`

	// setStatementTimeout sets the session's statement_timeout, in
	// milliseconds, which the server enforces even on a statement that waits for
	// a lock.
	setStatementTimeout = `SELECT set_config('statement_timeout', $1, false)`
)

// minStatementTimeout is the shortest statement timeout of a lease statement.
// The session keeps a statement's timeout until the next one sets its own, which
// it does under the last: one much shorter would fail it.
const minStatementTimeout = 10 * time.Millisecond

// runLease campaigns for the lease while this replica does not hold it, and
// renews it while it does, until ctx ends. A statement in flight when ctx ends
// runs to its own timeout, so that a takeover it made is known to leave, which
// releases it.
func (l *Leadership) runLease(ctx context.Context) {
	statements := context.WithoutCancel(ctx)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-l.wake:
		}
		if ctx.Err() != nil {
			return
		}
		var next time.Duration
		if t := l.current(); t != nil {
			next = l.renew(statements, t)
		} else {
			next = l.follow(ctx, statements)
		}
		timer.Reset(max(next, 0))
	}
}

// renew renews the lease of t, and returns when to renew it next: a third of
// the lease after this renewal's send, or a twelfth after a renewal that
// failed. A renewal that changes no row finds the lease moved, and steps down at
// once. No renewal is sent with a timeout past the step-down; the server counts
// its timeout from the statement's arrival, so a renewal can still land up to
// one network delay after the step-down.
func (l *Leadership) renew(ctx context.Context, t *term) time.Duration {
	timeout := l.renewalTimeout(t)
	if timeout <= 0 {
		// The step-down comes first, and wakes the loop.
		return l.renewEvery
	}
	sent := time.Now()
	renewed, err := l.renewRow(ctx, t.epoch, timeout)
	switch {
	case err != nil:
		l.leaseFailed(err, t.epoch, "database: renewing the lease failed: retrying until the step-down")
		return l.retryPause
	case !renewed:
		l.leaseOK()
		if l.stepDown(t) {
			l.logger.Warn().Int64("epoch", t.epoch).Msg("database: a renewal found the lease moved to another replica: stepping down")
			observe(l.observer.Lost)
		}
		return 0
	}
	l.leaseOK()
	l.renewed(t, sent)
	observe(l.observer.Renewed)
	return time.Until(sent.Add(l.renewEvery))
}

// renewalTimeout is the statement timeout of a renewal of t sent now: a sixth
// of the lease, cut to the time left before the step-down, so that no renewal
// lands after it; 0 when less than minStatementTimeout is left.
func (l *Leadership) renewalTimeout(t *term) time.Duration {
	l.mu.Lock()
	left := time.Until(t.deadline)
	l.mu.Unlock()
	if timeout := min(l.statementTimeout, left); timeout >= minStatementTimeout {
		return timeout
	}
	return 0
}

// follow reads the lease's holder, for CurrentLeader, and takes the lease when
// it is free or expired. It returns when to look again: at the expiry it read,
// a poll interval later at the latest.
func (l *Leadership) follow(ctx, statements context.Context) time.Duration {
	holder, live, err := l.readRow(statements)
	if err != nil {
		l.leaseFailed(err, 0, "database: reading the lease failed: retrying")
		return min(l.poll, l.retryPause)
	}
	l.see(holder)
	if live {
		l.leaseOK()
		return min(l.poll, time.Until(holder.until)+l.slack)
	}
	sent := time.Now()
	epoch, err := l.take(statements)
	switch {
	case err != nil:
		l.leaseFailed(err, 0, "database: taking the lease failed: retrying")
		return min(l.poll, l.retryPause)
	case epoch == 0:
		// Another replica took it first: read whose it is.
		l.leaseOK()
		return l.slack
	}
	l.leaseOK()
	if ctx.Err() != nil {
		// Run is ending: leave releases the lease, which this replica never leads.
		l.mu.Lock()
		l.held = epoch
		l.mu.Unlock()
		return 0
	}
	l.begin(epoch, sent)
	return time.Until(sent.Add(l.renewEvery))
}

// see keeps h as the lease's holder this replica read, for CurrentLeader: none
// when h is the zero seen. Another holder than the last wakes the watchers of
// the leader.
func (l *Leadership) see(h seen) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h.id != l.holder.id {
		l.leaderChanged()
	}
	l.holder = h
}

// readRow reads the lease's holder, with its until on this replica's clock, and
// whether its lease lasts; a missing row is no live holder. The time left is
// added to sent, the time before the statement went out, so that until is never
// later than the database's expiry.
func (l *Leadership) readRow(ctx context.Context) (seen, bool, error) {
	var (
		h    seen
		left int64
	)
	sent := time.Now()
	err := l.onLease(ctx, l.statementTimeout, func(ctx context.Context, conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, readHolder).Scan(&h.id, &h.address, &left)
		if errors.Is(err, sql.ErrNoRows) {
			left = 0
			return nil
		}
		return err
	})
	if err != nil || left <= 0 {
		return seen{}, false, err
	}
	h.until = sent.Add(time.Duration(left) * time.Microsecond)
	return h, true, nil
}

// take takes the lease when it is free or expired, and returns the epoch
// taken: 0 when another replica holds it. It runs one transaction, each of its
// statements under the statement timeout: lockForTakeover, which waits for the
// write transactions in flight, then takeLease. The transaction is READ
// COMMITTED, explicitly, as the store's are: the re-read of the row after a
// lock's wait relies on it, where a stricter level refuses a row that changed
// while it waited.
func (l *Leadership) take(ctx context.Context) (int64, error) {
	var epoch int64
	err := l.onLease(ctx, l.statementTimeout, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, lockForTakeover); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, takeLease, l.id, l.addr, l.lease.Microseconds()).Scan(&epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// renewRow renews the lease this replica holds at epoch, under timeout, and
// reports whether the row still held it.
func (l *Leadership) renewRow(ctx context.Context, epoch int64, timeout time.Duration) (bool, error) {
	var renewed bool
	err := l.onLease(ctx, timeout, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, renewLease, epoch, l.id, l.lease.Microseconds())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		renewed = n == 1
		return err
	})
	return renewed, err
}

// release ends now the lease this replica took at epoch, if the row still holds
// it: the others take it at their next look instead of at its expiry. A
// release that fails is logged.
func (l *Leadership) release(epoch int64) {
	var released bool
	err := l.onLease(context.Background(), l.statementTimeout, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, releaseLease, epoch, l.id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		released = n == 1
		return err
	})
	switch {
	case err != nil:
		l.logger.Warn().Err(err).Int64("epoch", epoch).
			Msg("database: releasing the lease failed: the other replicas take it over once it expires")
	case released:
		l.logger.Info().Int64("epoch", epoch).Msg("database: released the lease")
	}
}

// onLease runs fn on a connection of the lease's pool, its statements under
// timeout: the session's statement_timeout, set first, which the server enforces
// even on a statement that waits for a lock, and the context's deadline, a
// statement timeout later, for a server that does not answer. It returns fn's
// error as failed classifies it.
func (l *Leadership) onLease(ctx context.Context, timeout time.Duration, fn func(context.Context, *sql.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+l.statementTimeout)
	defer cancel()
	conn, err := l.leaseDB.Conn(ctx)
	if err != nil {
		return failed(ctx, err)
	}
	defer func() { _ = conn.Close() }()
	ms := strconv.FormatInt(max(timeout, minStatementTimeout).Milliseconds(), 10)
	if _, err := conn.ExecContext(ctx, setStatementTimeout, ms); err != nil {
		return failed(ctx, err)
	}
	if err := fn(ctx, conn); err != nil {
		return failed(ctx, err)
	}
	return nil
}

// leaseFailed logs a lease statement that failed, when it starts a run of
// failures: the next ones are quiet until one succeeds. An epoch of 0 is none.
func (l *Leadership) leaseFailed(err error, epoch int64, msg string) {
	if l.leaseFailing {
		return
	}
	l.leaseFailing = true
	event := l.logger.Warn().Err(err)
	if epoch != 0 {
		event = event.Int64("epoch", epoch)
	}
	event.Msg(msg)
}

// leaseOK ends a run of lease statements that failed, with a line.
func (l *Leadership) leaseOK() {
	if l.leaseFailing {
		l.leaseFailing = false
		l.logger.Info().Msg("database: the lease's statements succeed again")
	}
}
