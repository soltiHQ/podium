package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/service/agent"
)

// What the credential sweep logs: the stored credentials left, and the derived
// ones podium cannot use, under another secret or with none set.
const (
	storedCredentialsWarning = "agent credentials: stored tokens are left, kept raw in podium's state and in its backups: tokens agents chose while no enrollment secret was set, and tokens an earlier secret derived, which stay valid and which that secret still derives; resetting an agent's credential (POST /api/v1/agents/{id}/credential) while an enrollment secret is set moves it to a derived credential"
	otherSecretError         = "agent credentials: derived credentials of another enrollment secret than the current one: podium refuses these agents and makes no call to them until the secret they derive from is set again, each is re-provisioned with the token podium agent-token prints for it under the current secret, or its credential is reset (POST /api/v1/agents/{id}/credential), which issues a token derived under the current secret"
	noSecretError            = "agent credentials: derived credentials and no enrollment secret is set: podium refuses these agents and makes no call to them until the secret they derive from is set again, or each credential is reset (POST /api/v1/agents/{id}/credential), which issues a stored token"
)

// A memory start over the credentials an earlier version leaves, with the
// enrollment secret set, converts the stored credential whose token is the
// generation-0 token of its agent under that secret, and keeps the other one,
// whose agent chose its token: after the start the first is derived and stores
// no token, the second is stored. The rows are written before Run, which starts
// the agent-credentials runner that New wires. The start logs one WARN that
// counts the one stored credential left, no ERROR, as every derived credential
// derives from the current secret, and neither the secret nor a derived token.
func TestLegacyRowsConvertedAtStart(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	created := time.Unix(1700000000, 0)
	r := startApp(t, func(cfg *config.Config) { cfg.AgentAuth.EnrollSecret = secret }, func(a *App) {
		for agentID, token := range map[string]string{"agent-1": agent.DeriveToken(secret, "agent-1", 0), "agent-2": "token-the-agent-chose"} {
			c, err := model.NewStoredAgentCredential(agentID, token)
			if err != nil {
				t.Fatal(err)
			}
			c.SetCreatedAt(created)
			c.SetUpdatedAt(created)
			if err := a.store.UpsertAgentCredential(context.Background(), c); err != nil {
				t.Fatal(err)
			}
		}
	})

	ctx := context.Background()
	n := &clusterNode{id: "memory", logs: r.logs}
	stored := map[string]any{"level": "warn", "message": storedCredentialsWarning}
	waitFor(t, 10*time.Second, "the start converts agent-1 and counts the stored credentials", func() bool {
		c, err := r.app.store.GetAgentCredential(ctx, "agent-1")
		return err == nil && c.Derived() && len(n.logged(t, stored)) > 0
	})
	c, err := r.app.store.GetAgentCredential(ctx, "agent-1")
	if err != nil || c.Generation() != 0 || !bytes.Equal(c.SecretID(), agent.SecretID(secret)) || c.Token() != "" || !c.CreatedAt().Equal(created) {
		t.Fatalf("agent-1 after the start: %v, want derived at generation 0 under the secret, with no token and its enrollment time", err)
	}
	if c, err := r.app.store.GetAgentCredential(ctx, "agent-2"); err != nil || c.Derived() || c.Token() != "token-the-agent-chose" {
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
	if logs := r.logs.String(); strings.Contains(logs, secret) || strings.Contains(logs, agent.DeriveToken(secret, "agent-1", 0)) {
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
			n := &clusterNode{id: "node", logs: logs}
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
