package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	restv1 "github.com/soltiHQ/control-plane/api/rest/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// Three whole nodes run as main runs them, with one enrollment secret. An agent
// enrolls through one follower, and an operator who holds agents:reset resets
// its credential through another: the follower forwards the POST to the leader,
// which applies it as any write, and answers the new token, once, through the
// follower. Every node then holds the credential derived at the generation of
// the answer, the time of the reset in Unix seconds, the leader's access log
// holds the reset and the follower's does not, and every node holds the event
// of the reset. The old token is refused at the next sync, through a follower,
// and the new one is accepted. The token is in no node's log.
func TestResetThroughFollowerForwards(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	nodes := startWholeNodes(t, loopbackIPs(t, 3), 3, func(nodes []*clusterNode) {
		for _, n := range nodes {
			// Each replica reads its own environment: every one gets the same secret.
			n.cfg.AgentAuth.EnrollSecret = secret
		}
	}, "n1", "n2", "n3")
	leader, followers := readyNodes(t, nodes)
	enrolling, resetting := followers[1], followers[0]
	client := httpClient()

	oldToken := agent.DeriveToken(secret, "agent-1", 0)
	syncVia := func(via *clusterNode, token string) int {
		t.Helper()
		resp, body := send(t, client, http.MethodPost, "http://"+via.cfg.HTTPDiscovery.Addr+"/api/v1/discovery/sync", jsonType,
			`{"id":"agent-1","endpoint":"http://10.0.0.5:8085","endpointType":"ENDPOINT_TYPE_HTTP","apiVersion":1}`, token, nil)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("sync through %s: %d %q, want the leader's 200 or 401", via.id, resp.StatusCode, body)
		}
		return resp.StatusCode
	}
	if got := syncVia(enrolling, oldToken); got != http.StatusOK {
		t.Fatalf("agent-1 enrolling through %s: %d, want 200", enrolling.id, got)
	}
	generationHeld := func(generation uint32) func(storage.Storage) bool {
		return func(s storage.Storage) bool {
			c, err := s.GetAgentCredential(ctx, "agent-1")
			return err == nil && c.Derived() && c.Generation() == generation
		}
	}
	nodesHold(t, nodes, "the credential of agent-1 at generation 0", generationHeld(0))
	cookies := loginThrough(t, leader, resetting, nodes, "operator", enum.AgentsGet, enum.AgentsReset)
	// The event names the actor by the display name of the user.
	operator, err := leader.app.store.GetUserBySubject(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	operator.SetName("Operator")
	if err = leader.app.store.UpsertUser(ctx, operator); err != nil {
		t.Fatal(err)
	}

	const path = "/api/v1/agents/agent-1/credential"
	before := time.Now().Unix()
	resp, body := send(t, client, http.MethodPost, "http://"+resetting.cfg.HTTP.Addr+path, "", "", "", cookies)
	after := time.Now().Unix()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset through %s: %d %q, want the leader's 200", resetting.id, resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("the answer through %s carries Cache-Control %q, want no-store: it holds a token", resetting.id, got)
	}
	var issued restv1.ResetAgentCredentialResponse
	if err := json.Unmarshal([]byte(body), &issued); err != nil {
		t.Fatalf("answer %q: %v", body, err)
	}
	if issued.Generation == nil {
		t.Fatalf("answer %+v, want a generation", issued)
	}
	// The agent holds generation 0, so the leader issues the time of the reset in Unix seconds.
	generation := *issued.Generation
	if int64(generation) < before || int64(generation) > after {
		t.Fatalf("generation %d, want the time of the reset, from %d to %d", generation, before, after)
	}
	newToken := agent.DeriveToken(secret, "agent-1", generation)
	if issued.AgentID != "agent-1" || issued.Token != newToken {
		t.Fatalf("answer %+v, want agent-1 and its token at generation %d", issued, generation)
	}
	nodesHold(t, nodes, fmt.Sprintf("the credential of agent-1 at generation %d", generation), generationHeld(generation))

	// The access log of an API route runs behind the Leader middleware, so the
	// node that logs the request served it.
	waitFor(t, 5*time.Second, leader.id+" logs the reset", func() bool {
		return servedAPI(t, leader, http.MethodPost, path, http.StatusOK)
	})
	if servedAPI(t, resetting, http.MethodPost, path, http.StatusOK) {
		t.Fatalf("%s served the reset itself", resetting.id)
	}

	if got := syncVia(enrolling, oldToken); got != http.StatusUnauthorized {
		t.Fatalf("sync with the old token through %s: %d, want 401", enrolling.id, got)
	}
	if got := syncVia(enrolling, newToken); got != http.StatusOK {
		t.Fatalf("sync with the new token through %s: %d, want 200", enrolling.id, got)
	}

	for _, n := range nodes {
		waitFor(t, 5*time.Second, n.id+" holds the event of the reset", func() bool {
			for _, rec := range n.app.eventHub.RecentEvents(event.RingSize) {
				if rec.Kind == event.AgentCredentialReset && rec.Payload.ID == "agent-1" && rec.Payload.By == "Operator" {
					return true
				}
			}
			return false
		})
		for _, secretText := range []string{oldToken, newToken, secret} {
			if strings.Contains(n.logs.String(), secretText) {
				t.Errorf("the log of %s holds %q", n.id, secretText)
			}
		}
	}
}
