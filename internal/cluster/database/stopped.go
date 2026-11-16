package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrRunning is the error of CheckStopped while a replica of podium runs on the
// database.
var ErrRunning = errors.New("database: replicas of podium still run on the database")

// stoppedWindow is how long after its last heartbeat a replica counts as running
// for CheckStopped: three heartbeats, as long as Members keeps it.
const stoppedWindow = membersWindow * heartbeatInterval

// The statements of CheckStopped, read at the statement's own time of the
// database's clock, clock_timestamp(), as the transaction that runs them may be
// old. A duration of $1 is in microseconds.
const (
	// tableExists reads whether the table $1 exists, found through the search
	// path as an unqualified statement finds it.
	tableExists = `SELECT to_regclass($1) IS NOT NULL`

	// runningReplicas counts the replicas that heartbeated within $1.
	runningReplicas = `SELECT count(*) FROM podium_replicas WHERE last_seen > clock_timestamp() - $1::bigint * interval '1 microsecond'`

	// leaseHeld reads whether the lease is held: its row has not expired.
	leaseHeld = `SELECT EXISTS (SELECT 1 FROM podium_leader WHERE expires_at > clock_timestamp())`
)

// CheckStopped reports, through tx, whether no replica of podium runs on the
// database: none heartbeated its registry row within three heartbeats, and the
// lease is not held, the row free, released or expired, both by the database's
// clock. Otherwise it answers ErrRunning, naming how many replicas heartbeated
// and whether the lease is held. A table that does not exist holds no replica.
// A replica that stops cleanly deletes its row and releases the lease, so it no
// longer runs at once; one that crashed runs until its row is three heartbeats
// old and its lease expired.
//
// It is the guard of podium drop-schema, which sqlstore.DropSchema asks in its
// transaction, under the lock on the migrations, before it drops anything.
func CheckStopped(ctx context.Context, tx *sql.Tx) error {
	var found []string
	exists, err := tablePresent(ctx, tx, "podium_replicas")
	if err != nil {
		return err
	}
	if exists {
		var replicas int
		if err := tx.QueryRowContext(ctx, runningReplicas, stoppedWindow.Microseconds()).Scan(&replicas); err != nil {
			return fmt.Errorf("database: read the registry: %w", failed(ctx, err))
		}
		switch {
		case replicas == 1:
			found = append(found, fmt.Sprintf("1 replica heartbeated within %s", stoppedWindow))
		case replicas > 1:
			found = append(found, fmt.Sprintf("%d replicas heartbeated within %s", replicas, stoppedWindow))
		}
	}
	if exists, err = tablePresent(ctx, tx, "podium_leader"); err != nil {
		return err
	}
	if exists {
		var held bool
		if err := tx.QueryRowContext(ctx, leaseHeld).Scan(&held); err != nil {
			return fmt.Errorf("database: read the lease: %w", failed(ctx, err))
		}
		if held {
			found = append(found, "the lease is held")
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRunning, strings.Join(found, ", and "))
}

// tablePresent reads whether the table name exists.
func tablePresent(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, tableExists, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("database: look for %s: %w", name, failed(ctx, err))
	}
	return exists, nil
}
