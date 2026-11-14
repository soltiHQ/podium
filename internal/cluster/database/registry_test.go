package database

import (
	"bytes"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// registered reports whether the registry holds a row for the replica id.
func registered(t *testing.T, admin *sql.DB, id string) bool {
	t.Helper()
	var n int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM podium_replicas WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read the registry: %v", err)
	}
	return n > 0
}

// heartbeatAs writes the registry row of peer, last seen now, as its own
// heartbeat would.
func heartbeatAs(t *testing.T, admin *sql.DB, peer cluster.Peer) {
	t.Helper()
	_, err := admin.ExecContext(t.Context(), `INSERT INTO podium_replicas (id, advertise_addr, digests, last_seen)
VALUES ($1, $2, $3, now())
ON CONFLICT (id) DO UPDATE SET last_seen = now()`, peer.ID, peer.Address, make([]byte, digestsSize))
	if err != nil {
		t.Fatalf("heartbeat as %s: %v", peer.ID, err)
	}
}

// flat is the encoding of settings' digests in the registry: the groups in
// order.
func flat(settings cluster.SharedSettings) []byte {
	var b []byte
	for _, group := range settings.Digests() {
		b = append(b, group[:]...)
	}
	return b
}

// TestMembersFromRegistry checks that each replica's Members lists the
// replicas seen within three heartbeats, by their IDs and advertise addresses,
// with the digests of their shared settings in the registry; that a replica that
// stops heartbeating drops out three heartbeats after its last; and that one
// that stops cleanly deletes its row.
func TestMembersFromRegistry(t *testing.T) {
	const heartbeat = 200 * time.Millisecond
	databaseURL, admin := migratedDatabase(t)
	settings := map[string]cluster.SharedSettings{
		"replica-a": {JWTSecret: "secret-a", Issuer: "podium"},
		"replica-b": {JWTSecret: "secret-b", Issuer: "podium"},
	}
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{heartbeat: heartbeat, settings: settings["replica-a"]})
	b := newReplica(t, databaseURL, "replica-b", replicaConfig{heartbeat: heartbeat, settings: settings["replica-b"]})
	a.start(t)
	b.start(t)

	both := []cluster.Peer{{ID: a.id, Address: a.addr}, {ID: b.id, Address: b.addr}}
	waitFor(t, 5*time.Second, "both replicas among the members of each", func() bool {
		return slices.Equal(a.Members(), both) && slices.Equal(b.Members(), both)
	})
	for id, s := range settings {
		var stored []byte
		if err := admin.QueryRowContext(t.Context(), `SELECT digests FROM podium_replicas WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatalf("read the digests of %s: %v", id, err)
		}
		if !bytes.Equal(stored, flat(s)) {
			t.Errorf("the registry holds digests of %s other than those of its settings", id)
		}
	}

	// A replica that stops heartbeating without leaving, as one that crashed,
	// drops out three heartbeats after its last.
	ghost := cluster.Peer{ID: "replica-ghost", Address: "replica-ghost.podium.test:7000"}
	var last time.Time
	waitFor(t, 5*time.Second, "the ghost among the members", func() bool {
		last = time.Now()
		heartbeatAs(t, admin, ghost)
		return slices.Contains(a.Members(), ghost)
	})
	waitFor(t, 5*time.Second, "the ghost out of the members", func() bool { return !slices.Contains(a.Members(), ghost) })
	if gone := time.Since(last); gone < 2*heartbeat || gone > 4*heartbeat {
		t.Errorf("the ghost dropped out %s after its last heartbeat, want after three heartbeats, %s", gone, 3*heartbeat)
	}

	// A replica that stops cleanly deletes its row, and drops out.
	if err := b.stop(); err != nil {
		t.Fatalf("replica-b's Run: %v", err)
	}
	if registered(t, admin, b.id) {
		t.Error("replica-b's registry row survived its stop")
	}
	alone := []cluster.Peer{{ID: a.id, Address: a.addr}}
	waitFor(t, 5*time.Second, "replica-a alone among its members", func() bool { return slices.Equal(a.Members(), alone) })
}

// TestRegistryPrunedByTheLeader checks that the leader deletes the registry
// rows unseen for an hour and keeps the younger ones, which are no members
// either.
func TestRegistryPrunedByTheLeader(t *testing.T) {
	databaseURL, admin := migratedDatabase(t)
	for _, row := range []struct{ id, age string }{
		{"replica-old", "2 hours"},
		{"replica-recent", "30 minutes"},
	} {
		_, err := admin.ExecContext(t.Context(), `INSERT INTO podium_replicas (id, advertise_addr, digests, last_seen)
VALUES ($1, $2, $3, now() - $4::interval)`, row.id, row.id+".podium.test:7000", make([]byte, digestsSize), row.age)
		if err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	a := newReplica(t, databaseURL, "replica-a", replicaConfig{heartbeat: 100 * time.Millisecond, pruneEvery: 20 * time.Millisecond})
	a.start(t)
	waitFor(t, 5*time.Second, "replica-a leading", a.AmLeader)

	waitFor(t, 5*time.Second, "the row unseen for two hours pruned", func() bool { return !registered(t, admin, "replica-old") })
	if !registered(t, admin, "replica-recent") {
		t.Error("the leader pruned a row unseen for 30 minutes")
	}
	for _, p := range a.Members() {
		if p.ID == "replica-recent" {
			t.Error("a replica unseen for 30 minutes is a member")
		}
	}
}
