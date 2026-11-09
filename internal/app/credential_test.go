package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	raftv1 "github.com/soltiHQ/control-plane/api/gen/solti/raft/v1"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage/durable"
	"github.com/soltiHQ/control-plane/internal/storage/snapshot"
)

// What the credential sweep logs: the stored credentials left, and the derived
// ones podium cannot use, under another secret or with none set.
const (
	storedCredentialsWarning = "agent credentials: stored tokens are left, kept raw in podium's state and in its backups: tokens agents chose while no enrollment secret was set, and tokens an earlier secret derived, which stay valid and which that secret still derives; resetting an agent's credential (POST /api/v1/agents/{id}/credential) while an enrollment secret is set moves it to a derived credential"
	otherSecretError         = "agent credentials: derived credentials of another enrollment secret than the current one: podium refuses these agents and makes no call to them until the secret they derive from is set again, each is re-provisioned with the token podium agent-token prints for it under the current secret, or its credential is reset (POST /api/v1/agents/{id}/credential), which issues a token derived under the current secret"
	noSecretError            = "agent credentials: derived credentials and no enrollment secret is set: podium refuses these agents and makes no call to them until the secret they derive from is set again, or each credential is reset (POST /api/v1/agents/{id}/credential), which issues a stored token"
)

// writeV3Store writes, in a fresh private data directory, the standalone store
// that the previous version leaves after a snapshot: the log layout of
// internal/storage/durable, format 2, whose snapshot at index 0 is a version-3
// snapshot of creds, and an empty log. It returns the directory.
func writeV3Store(t *testing.T, creds ...*raftv1.AgentCredentialMsg) string {
	t.Helper()
	blob, err := proto.MarshalOptions{Deterministic: true}.Marshal(&raftv1.Snapshot{
		Header:           &raftv1.SnapshotHeader{Magic: snapshot.Magic, Version: 3},
		AgentCredentials: creds,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, durable.FileName), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	number := func(n uint64) []byte { return binary.BigEndian.AppendUint64(nil, n) }
	sum := sha256.Sum256(blob)
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket([]byte("meta"))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte("format"), number(2)); err != nil {
			return err
		}
		if err := meta.Put([]byte("snapshot_index"), number(0)); err != nil {
			return err
		}
		snap, err := tx.CreateBucket([]byte("snapshot"))
		if err != nil {
			return err
		}
		if err := snap.Put([]byte("blob"), blob); err != nil {
			return err
		}
		if err := snap.Put([]byte("sha256"), sum[:]); err != nil {
			return err
		}
		_, err = tx.CreateBucket([]byte("log"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A standalone start over a store the previous version wrote, with the
// enrollment secret set, converts the stored credential whose token is the
// generation-0 token of its agent under that secret, and keeps the other one,
// whose agent chose its token: after the start the first is derived and stores
// no token, the second is stored. The start logs one WARN that counts the one
// stored credential left, and no ERROR, as every derived credential derives
// from the current secret.
func TestLegacyRowsConvertedAtStart(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	created := time.Unix(1700000000, 0).UnixNano()
	dir := writeV3Store(t,
		&raftv1.AgentCredentialMsg{AgentId: "agent-1", Token: agent.DeriveToken(secret, "agent-1", 0), CreatedAtNs: created, UpdatedAtNs: created},
		&raftv1.AgentCredentialMsg{AgentId: "agent-2", Token: "token-the-agent-chose", CreatedAtNs: created, UpdatedAtNs: created},
	)
	cfg := config.Default()
	cfg.Cluster.Standalone.DataDir = dir
	cfg.Auth.JWTSecret = strings.Repeat("legacy-credential-test-secret", 2)
	cfg.AgentAuth.EnrollSecret = secret
	cfg.HTTP.Addr, cfg.HTTPDiscovery.Addr, cfg.GRPC.Addr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	n := &clusterNode{id: "standalone", cfg: cfg, logs: &syncBuffer{}}
	n.start(t)

	ctx := context.Background()
	stored := map[string]any{"level": "warn", "message": storedCredentialsWarning}
	waitFor(t, 10*time.Second, "the start converts agent-1 and counts the stored credentials", func() bool {
		c, err := n.app.store.GetAgentCredential(ctx, "agent-1")
		return err == nil && c.Derived() && len(n.logged(t, stored)) > 0
	})
	c, err := n.app.store.GetAgentCredential(ctx, "agent-1")
	if err != nil || c.Generation() != 0 || !bytes.Equal(c.SecretID(), agent.SecretID(secret)) || c.Token() != "" || c.CreatedAt().UnixNano() != created {
		t.Fatalf("agent-1 after the start: %v, want derived at generation 0 under the secret, with no token and its enrollment time", err)
	}
	if c, err := n.app.store.GetAgentCredential(ctx, "agent-2"); err != nil || c.Derived() || c.Token() != "token-the-agent-chose" {
		t.Fatalf("agent-2 after the start: %v, want it stored with its token", err)
	}
	warnings := n.logged(t, stored)
	if len(warnings) != 1 || warnings[0].fields["stored_credentials"] != float64(1) {
		t.Fatalf("stored-credential warnings %v, want one that counts 1", warnings)
	}
	for _, message := range []string{otherSecretError, noSecretError} {
		if lines := n.logged(t, map[string]any{"message": message}); len(lines) != 0 {
			t.Fatalf("the start logged %v, want no unusable credential", lines)
		}
	}
	if strings.Contains(n.logs.String(), secret) || strings.Contains(n.logs.String(), agent.DeriveToken(secret, "agent-1", 0)) {
		t.Fatal("the log holds the secret or a token")
	}
}

// A sweep logs what it found, a line per finding, each with its count and no
// more: an INFO for the stored credentials it converted, a WARN for the stored
// ones left, and an ERROR for the derived ones podium cannot use, which says
// whether no secret is set or the credentials derive from another one. A sweep
// that found nothing logs nothing.
func TestCredentialSweepLogsItsFindings(t *testing.T) {
	type line struct {
		level, message, field string
		count                 int
	}
	const converted = "agent credentials: stored tokens that the enrollment secret derives are now derived credentials: no snapshot or log entry written from now on holds them"
	for _, tc := range []struct {
		name      string
		res       agent.CredentialSweep
		secretSet bool
		want      []line
	}{
		{"nothing", agent.CredentialSweep{}, true, nil},
		{"converted", agent.CredentialSweep{Converted: 3}, true, []line{{"info", converted, "converted_credentials", 3}}},
		{"stored left", agent.CredentialSweep{Stored: 2}, false, []line{{"warn", storedCredentialsWarning, "stored_credentials", 2}}},
		{"another secret", agent.CredentialSweep{Unusable: 4}, true, []line{{"error", otherSecretError, "unusable_credentials", 4}}},
		{"no secret", agent.CredentialSweep{Unusable: 1}, false, []line{{"error", noSecretError, "unusable_credentials", 1}}},
		{"all three", agent.CredentialSweep{Converted: 1, Stored: 1, Unusable: 1}, true, []line{
			{"info", converted, "converted_credentials", 1},
			{"warn", storedCredentialsWarning, "stored_credentials", 1},
			{"error", otherSecretError, "unusable_credentials", 1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			logCredentialSweep(zerolog.New(logs), tc.res, tc.secretSet)
			var got []line
			n := &clusterNode{id: "standalone", logs: logs}
			for _, l := range n.logged(t, map[string]any{}) {
				var counted line
				for _, field := range []string{"converted_credentials", "stored_credentials", "unusable_credentials"} {
					if v, ok := l.fields[field].(float64); ok {
						counted = line{field: field, count: int(v)}
					}
				}
				counted.level, _ = l.fields["level"].(string)
				counted.message, _ = l.fields["message"].(string)
				got = append(got, counted)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("logged %v, want %v", got, tc.want)
			}
		})
	}
}
