package database

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// digestsSize is the size of a registry row's digests: cluster.Digests, the
// groups in order.
const digestsSize = cluster.GroupCount * sha256.Size

// membersWindow is how many heartbeats a replica stays a member after its last.
const membersWindow = 3

// The registry's statements, on the primary's pool. now() is the database's
// clock. A duration of $1 is in microseconds.
const (
	// heartbeatReplica writes this replica's row, last seen now.
	heartbeatReplica = `INSERT INTO podium_replicas (id, advertise_addr, digests, last_seen)
VALUES ($1, $2, $3, now())
ON CONFLICT (id) DO UPDATE
   SET advertise_addr = excluded.advertise_addr,
       digests = excluded.digests,
       last_seen = excluded.last_seen`

	// readReplicas reads the replicas seen within $1, by ID, each with the time
	// since its last heartbeat, in microseconds.
	readReplicas = `SELECT id, advertise_addr, floor(extract(epoch FROM now() - last_seen) * 1000000)::bigint
  FROM podium_replicas
 WHERE last_seen > now() - $1::bigint * interval '1 microsecond'
 ORDER BY id COLLATE "C"`

	// pruneReplicas deletes the rows unseen for longer than $1.
	pruneReplicas = `DELETE FROM podium_replicas WHERE last_seen < now() - $1::bigint * interval '1 microsecond'`

	// deleteReplica deletes the row of the replica $1.
	deleteReplica = `DELETE FROM podium_replicas WHERE id = $1`
)

// member is a replica the registry listed, a member until a time.
type member struct {
	peer cluster.Peer
	// until is when it stops being a member without a later heartbeat, by this
	// replica's clock: three heartbeats after its last.
	until time.Time
}

// Members implements cluster.Membership: the replicas seen within three
// heartbeats, by ID, each with its ID and advertise address, from the list the
// last heartbeat read, which a call filters by the time each was seen and
// never reads anew. A replica whose heartbeats fail drops out of its own list
// too.
func (l *Leadership) Members() []cluster.Peer {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	peers := make([]cluster.Peer, 0, len(l.members))
	for _, m := range l.members {
		if now.Before(m.until) {
			peers = append(peers, m.peer)
		}
	}
	return peers
}

// runRegistry heartbeats this replica's row and reads the members every
// heartbeat, and prunes the rows unseen for an hour every minute while this
// replica leads, until ctx ends.
func (l *Leadership) runRegistry(ctx context.Context) {
	ticker := time.NewTicker(l.heartbeat)
	defer ticker.Stop()
	var pruned time.Time
	for {
		l.beat(ctx)
		if l.AmLeader() && time.Since(pruned) >= l.pruneEvery {
			l.prune(ctx)
			pruned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// beat writes this replica's row, last seen now, and reads the members: each a
// member until three heartbeats after its last, counted from before the read
// went out, so that none stays longer than the database's view allows. Both
// statements are bounded by those three heartbeats; a failure keeps the last
// list, which ages out by itself.
func (l *Leadership) beat(ctx context.Context) {
	window := membersWindow * l.heartbeat
	stmt, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	members, err := l.heartbeatAndRead(stmt, window)
	if err != nil {
		err = failed(stmt, err)
		if ctx.Err() == nil && !l.registryFailing {
			l.registryFailing = true
			l.logger.Warn().Err(err).Msg("database: the registry heartbeat failed: retrying every heartbeat")
		}
		return
	}
	if l.registryFailing {
		l.registryFailing = false
		l.logger.Info().Msg("database: the registry heartbeat succeeds again")
	}
	l.mu.Lock()
	l.members = members
	l.mu.Unlock()
}

// heartbeatAndRead writes this replica's row and reads the replicas seen
// within window.
func (l *Leadership) heartbeatAndRead(ctx context.Context, window time.Duration) ([]member, error) {
	if _, err := l.db.ExecContext(ctx, heartbeatReplica, l.id, l.addr, l.digests); err != nil {
		return nil, err
	}
	sent := time.Now()
	rows, err := l.db.QueryContext(ctx, readReplicas, window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var members []member
	for rows.Next() {
		var (
			m   member
			age int64
		)
		if err := rows.Scan(&m.peer.ID, &m.peer.Address, &age); err != nil {
			return nil, err
		}
		m.until = sent.Add(window - time.Duration(age)*time.Microsecond)
		members = append(members, m)
	}
	return members, rows.Err()
}

// prune deletes the registry rows unseen for an hour: those of replicas that
// stopped without deleting their own.
func (l *Leadership) prune(ctx context.Context) {
	stmt, cancel := context.WithTimeout(ctx, membersWindow*l.heartbeat)
	defer cancel()
	res, err := l.db.ExecContext(stmt, pruneReplicas, l.pruneAfter.Microseconds())
	if err != nil {
		if ctx.Err() == nil {
			l.logger.Warn().Err(failed(stmt, err)).Msg("database: pruning the registry failed: retrying in a minute")
		}
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		l.logger.Info().Int64("rows", n).Dur("unseen_for", l.pruneAfter).Msg("database: pruned the registry rows of replicas long gone")
	}
}

// deregister deletes this replica's registry row, as Run ends. A delete that
// fails is logged: the leader prunes the row after an hour, and it is no
// member after three heartbeats.
func (l *Leadership) deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), membersWindow*l.heartbeat)
	defer cancel()
	if _, err := l.db.ExecContext(ctx, deleteReplica, l.id); err != nil {
		l.logger.Warn().Err(failed(ctx, err)).Msg("database: deleting this replica's registry row failed: the leader prunes it after an hour")
	}
}
