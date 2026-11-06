package raft

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/service/agent"
)

// credentialSecret is the enrollment secret of the tests below.
const credentialSecret = "0123456789abcdef0123456789abcdef"

// With the enrollment secret set, an agent's credential is derived, and no file
// under a raft node's data directory holds its token, as hex or as raw bytes:
// not the log entries of its enrollment and syncs, not a snapshot, not an entry
// after it. A token enrolled without the secret is stored, and the same search
// finds it, so the search sees what the node writes.
func TestDerivedCredentialStoresNoToken(t *testing.T) {
	ctx := context.Background()
	dir := privateTestDir(t)
	p, _, hub := openRestartProfile(t, dir)
	defer hub.Close()
	svc := agent.New(p.Store(), zerolog.Nop(), agent.WithEnrollSecret(credentialSecret))
	tokens := map[string]string{}
	enroll := func(id string) {
		t.Helper()
		token := agent.DeriveToken(credentialSecret, id, 0)
		for range 2 { // the enrolling sync, then a later one
			if err := svc.VerifyOrEnrollToken(ctx, id, token); err != nil {
				_ = p.Shutdown()
				t.Fatalf("sync of %s: %v", id, err)
			}
		}
		tokens[id] = token
	}
	for i := range 4 {
		enroll(fmt.Sprintf("agent-%d", i))
	}
	const stored = "token-of-an-agent-enrolled-without-the-secret"
	if err := agent.New(p.Store(), zerolog.Nop()).VerifyOrEnrollToken(ctx, "tofu-agent", stored); err != nil {
		_ = p.Shutdown()
		t.Fatal(err)
	}
	if err := p.raft.Snapshot().Error(); err != nil {
		_ = p.Shutdown()
		t.Fatal(err)
	}
	enroll("agent-after-the-snapshot")
	if err := p.Shutdown(); err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = data
		return err
	}); err != nil {
		t.Fatal(err)
	}
	holding := func(token string) []string {
		raw, _ := hex.DecodeString(token)
		var paths []string
		for path, data := range files {
			if bytes.Contains(data, []byte(token)) || (len(raw) > 0 && bytes.Contains(data, raw)) {
				paths = append(paths, path)
			}
		}
		return paths
	}
	for id, token := range tokens {
		if paths := holding(token); len(paths) != 0 {
			t.Errorf("the token of %s is in %v", id, paths)
		}
	}
	if paths := holding(stored); len(paths) == 0 {
		t.Fatal("the search found no stored token either: it cannot tell a token at rest")
	}
}
