package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
)

// clusterAdmin is the cluster administration of the raft node an apiFixture
// plays: n1 leads the voters n1, n2 and n3, each at 10.0.0.<i>:7000, in a
// configuration at index 7. A test changes what it holds and the errors it
// answers with; it records the changes it was asked for.
type clusterAdmin struct {
	mu sync.Mutex

	role             string
	leaderID, leader string
	conf             cluster.Configuration

	probeErr, addErr, removeErr, transferErr error

	changes   []string
	transfers int
}

func newClusterAdmin() *clusterAdmin {
	a := &clusterAdmin{role: cluster.RoleLeader, leaderID: "n1", leader: "10.0.0.1:7000", conf: cluster.Configuration{Index: 7}}
	for i := 1; i <= 3; i++ {
		a.conf.Servers = append(a.conf.Servers, cluster.Server{ID: fmt.Sprintf("n%d", i), Address: fmt.Sprintf("10.0.0.%d:7000", i), Suffrage: cluster.Voter})
	}
	return a
}

func (a *clusterAdmin) NodeID() string { return "n1" }

func (a *clusterAdmin) Role() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.role
}

func (a *clusterAdmin) Leader() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.leaderID, a.leader
}

func (a *clusterAdmin) LastIndex() uint64 { return 42 }

func (a *clusterAdmin) Servers() []cluster.Server {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.conf.Servers)
}

func (a *clusterAdmin) Configuration() (cluster.Configuration, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cluster.Configuration{Index: a.conf.Index, Servers: slices.Clone(a.conf.Servers)}, nil
}

func (a *clusterAdmin) Probe(context.Context, string) error { return a.probeErr }

func (a *clusterAdmin) AddVoter(id, address string, prevIndex uint64) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.changes = append(a.changes, fmt.Sprintf("add %s %s at %d", id, address, prevIndex))
	return prevIndex + 1, a.addErr
}

func (a *clusterAdmin) RemoveServer(id string, prevIndex uint64) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.changes = append(a.changes, fmt.Sprintf("remove %s at %d", id, prevIndex))
	return prevIndex + 1, a.removeErr
}

func (a *clusterAdmin) TransferLeadership() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.transfers++
	if a.transferErr != nil {
		return a.transferErr
	}
	a.role, a.leaderID, a.leader = cluster.RoleFollower, "n2", "10.0.0.2:7000"
	return nil
}

// clusterRoutes are the three cluster routes, each with a request that a
// holder of its permission gets through, and its answer then.
var clusterRoutes = []struct {
	method, target, body string
	perm                 enum.Permission
	code                 int
}{
	{http.MethodGet, "/api/v1/cluster", "", enum.ClusterGet, http.StatusOK},
	{http.MethodPost, "/api/v1/cluster/members", `{"id":"n4","address":"10.0.0.4:7000"}`, enum.ClusterManage, http.StatusNoContent},
	{http.MethodDelete, "/api/v1/cluster/members/n3?force=true", "", enum.ClusterManage, http.StatusNoContent},
}

// The cluster routes need cluster:get to read and cluster:manage to change. Of
// the built-in roles only Admin grants them: its users get through all three
// routes, and the users of every other built-in role through none. A user who
// holds one of the two permissions gets through its routes only.
func TestClusterRoutesNeedPermission(t *testing.T) {
	for _, br := range enum.BuiltinRoles {
		holds := slices.Contains(br.Permissions, enum.ClusterGet) || slices.Contains(br.Permissions, enum.ClusterManage)
		if holds != (br.ID == enum.RoleAdminID) {
			t.Errorf("the built-in role %s holds the cluster permissions: %v", br.Name, holds)
		}
		for _, rt := range clusterRoutes {
			t.Run(br.Name+" "+rt.method+" "+rt.target, func(t *testing.T) {
				f := newAPIFixture(t)
				w := f.do(rt.method, rt.target, rt.body, f.user(t, "user-"+br.ID, br.ID))
				want := http.StatusForbidden
				if br.ID == enum.RoleAdminID {
					want = rt.code
				}
				if w.Code != want {
					t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), want)
				}
			})
		}
	}
	for _, p := range []enum.Permission{enum.ClusterGet, enum.ClusterManage} {
		for _, rt := range clusterRoutes {
			t.Run(string(p)+" alone "+rt.method+" "+rt.target, func(t *testing.T) {
				f := newAPIFixture(t)
				w := f.do(rt.method, rt.target, rt.body, &identity.Identity{UserID: "operator", Permissions: []enum.Permission{p}})
				want := http.StatusForbidden
				if rt.perm == p {
					want = rt.code
				}
				if w.Code != want {
					t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

// GET /api/v1/cluster answers the cluster as the node sees it: its ID and role,
// the leader it knows by ID and raft address, every member with its address and
// suffrage, and the last index of its log.
func TestClusterStatusAnswer(t *testing.T) {
	f := newAPIFixture(t)
	f.cluster.role, f.cluster.leaderID, f.cluster.leader = cluster.RoleFollower, "n2", "10.0.0.2:7000"
	w := f.do(http.MethodGet, "/api/v1/cluster", "", &identity.Identity{UserID: "reader", Permissions: []enum.Permission{enum.ClusterGet}})
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s, want 200", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	member := func(i int) map[string]any {
		return map[string]any{"id": fmt.Sprintf("n%d", i), "address": fmt.Sprintf("10.0.0.%d:7000", i), "suffrage": "voter"}
	}
	want := map[string]any{
		"node_id": "n1", "role": "follower", "leader_id": "n2", "leader_address": "10.0.0.2:7000",
		"members": []any{member(1), member(2), member(3)}, "last_index": float64(42),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GET /api/v1/cluster = %v, want %v", got, want)
	}
}

// A membership change answers 204 once raft made it, and every refusal has its
// answer: 400 for a body without an id or an address, or an address that is not
// host:port, 415 for a body that is not JSON, 404 for the removal of an unknown
// member, 409 for an id or an address in use, the force guard, the last voter
// and a configuration that changed since it was read, 422 for an address where
// no node answers, naming it, and 503 with X-Leader on a follower or for a change
// that lost leadership while it committed. The rules refuse before raft is asked
// for any change; a configuration that changed is raft's refusal of the change
// it was asked for. The leader asked to remove itself hands its leadership over,
// removes nothing and answers 503 with X-Leader and a message to repeat the
// request; a transfer that fails answers 503 without it.
func TestClusterRouteAnswers(t *testing.T) {
	const (
		post, del = http.MethodPost, http.MethodDelete
		members   = "/api/v1/cluster/members"
		jsonBody  = "application/json"
		newMember = `{"id":"n4","address":"10.0.0.4:7000"}`
		hint      = "10.0.0.2:8080"
		invalid   = "A member needs an id without /, =, a comma or surrounding spaces, and an address host:port with a port from 1 to 65535."
	)
	operator := &identity.Identity{UserID: "operator", Permissions: []enum.Permission{enum.ClusterGet, enum.ClusterManage}}
	follower := func(a *clusterAdmin) { a.role, a.leaderID, a.leader = cluster.RoleFollower, "n2", "10.0.0.2:7000" }
	for _, tc := range []struct {
		name                        string
		method, target, ctype, body string
		setup                       func(*clusterAdmin)
		code                        int
		message, leader             string // the error body's message; X-Leader
		changes                     []string
		transfers                   int
	}{
		{
			name: "add", method: post, target: members, ctype: jsonBody, body: newMember,
			code: http.StatusNoContent, changes: []string{"add n4 10.0.0.4:7000 at 7"},
		},
		{
			name: "add without an id", method: post, target: members, ctype: jsonBody, body: `{"address":"10.0.0.4:7000"}`,
			code: http.StatusBadRequest, message: invalid,
		},
		{
			name: "add without an address", method: post, target: members, ctype: jsonBody, body: `{"id":"n4"}`,
			code: http.StatusBadRequest, message: invalid,
		},
		{
			name: "add an address that is not host:port", method: post, target: members, ctype: jsonBody, body: `{"id":"n4","address":"10.0.0.4"}`,
			code: http.StatusBadRequest, message: invalid,
		},
		{
			name: "add a malformed body", method: post, target: members, ctype: jsonBody, body: `{"id":`,
			code: http.StatusBadRequest, message: "invalid request",
		},
		{
			name: "add a body that is not JSON", method: post, target: members, ctype: "text/plain", body: newMember,
			code: http.StatusUnsupportedMediaType, message: "Content-Type must be application/json.",
		},
		{
			name: "add the id of a member", method: post, target: members, ctype: jsonBody, body: `{"id":"n2","address":"10.0.0.4:7000"}`,
			code: http.StatusConflict, message: "A member of the cluster has this id already.",
		},
		{
			name: "add the address of a member", method: post, target: members, ctype: jsonBody, body: `{"id":"n4","address":"10.0.0.2:7000"}`,
			code: http.StatusConflict, message: "Another member of the cluster uses this address.",
		},
		{
			name: "add where no node answers", method: post, target: members, ctype: jsonBody, body: newMember,
			setup: func(a *clusterAdmin) {
				a.probeErr = fmt.Errorf("raft: probe 10.0.0.4:7000: %w: connection refused", cluster.ErrPeerUnreachable)
			},
			code: http.StatusUnprocessableEntity, message: "No raft node of this cluster answers at 10.0.0.4:7000: check the address, that the node runs, and its certificate. Nothing was changed.",
		},
		{
			name: "add against a configuration that changed", method: post, target: members, ctype: jsonBody, body: newMember,
			setup: func(a *clusterAdmin) { a.addErr = fmt.Errorf("raft: %w", cluster.ErrConfigurationChanged) },
			code:  http.StatusConflict, message: "configuration changed, retry", changes: []string{"add n4 10.0.0.4:7000 at 7"},
		},
		{
			name: "add on a follower", method: post, target: members, ctype: jsonBody, body: newMember, setup: follower,
			code: http.StatusServiceUnavailable, message: "service unavailable", leader: hint,
		},
		{
			name: "remove", method: del, target: members + "/n3?force=true",
			code: http.StatusNoContent, changes: []string{"remove n3 at 7"},
		},
		{
			name: "remove without force", method: del, target: members + "/n3",
			code: http.StatusConflict, message: "The removal would leave fewer than 3 voters: repeat it with force=true to remove the member anyway.",
		},
		{
			name: "remove with a force that is no boolean", method: del, target: members + "/n3?force=yes",
			code: http.StatusBadRequest, message: "force must be true or false.",
		},
		{
			name: "remove the last voter", method: del, target: members + "/n1?force=true",
			setup: func(a *clusterAdmin) { a.conf.Servers = a.conf.Servers[:1] },
			code:  http.StatusConflict, message: "The last voter cannot be removed.",
		},
		{
			name: "remove an unknown member", method: del, target: members + "/n9?force=true",
			code: http.StatusNotFound, message: "not found",
		},
		{
			name: "remove against a configuration that changed", method: del, target: members + "/n3?force=true",
			setup: func(a *clusterAdmin) { a.removeErr = fmt.Errorf("raft: %w", cluster.ErrConfigurationChanged) },
			code:  http.StatusConflict, message: "configuration changed, retry", changes: []string{"remove n3 at 7"},
		},
		{
			name: "remove the leader", method: del, target: members + "/n1?force=true",
			code: http.StatusServiceUnavailable, leader: hint, transfers: 1,
			message: "This node handed its leadership over and is still a member: repeat the request, at the leader that X-Leader names, to remove it.",
		},
		{
			name: "remove the leader, the transfer fails", method: del, target: members + "/n1?force=true",
			setup: func(a *clusterAdmin) { a.transferErr = errors.New("leadership transfer timeout") },
			code:  http.StatusServiceUnavailable, transfers: 1,
			message: "The leadership transfer failed, so nothing was removed: remove any dead member first, then retry.",
		},
		{
			name: "remove on a follower", method: del, target: members + "/n3?force=true", setup: follower,
			code: http.StatusServiceUnavailable, message: "service unavailable", leader: hint,
		},
		{
			name: "remove that lost leadership while it committed", method: del, target: members + "/n3?force=true",
			setup: func(a *clusterAdmin) { a.removeErr = fmt.Errorf("raft: %w", cluster.ErrLeadershipLost) },
			code:  http.StatusServiceUnavailable, message: "service unavailable", leader: hint, changes: []string{"remove n3 at 7"},
		},
		{
			name: "remove that failed", method: del, target: members + "/n3?force=true",
			setup: func(a *clusterAdmin) { a.removeErr = errors.New("boom") },
			code:  http.StatusServiceUnavailable, message: "service unavailable", changes: []string{"remove n3 at 7"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPIFixture(t)
			if tc.setup != nil {
				tc.setup(f.cluster)
			}
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.ctype != "" {
				r.Header.Set("Content-Type", tc.ctype)
			}
			// The Leader middleware tells where the leader is; it is asked only for a
			// write that met the wrong leadership.
			ctx := httpctx.WithLeader(r.Context(), func() string { return hint })
			r = r.WithContext(transportctx.WithIdentity(ctx, operator))
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)

			if w.Code != tc.code {
				t.Fatalf("HTTP %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if tc.message != "" {
				var body struct{ Message string }
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Message != tc.message {
					t.Fatalf("message %q (%v), want %q", body.Message, err, tc.message)
				}
			}
			if got := w.Header().Get(response.LeaderHeader); got != tc.leader {
				t.Fatalf("%s = %q, want %q", response.LeaderHeader, got, tc.leader)
			}
			if !slices.Equal(f.cluster.changes, tc.changes) || f.cluster.transfers != tc.transfers {
				t.Fatalf("changes %v and %d transfers, want %v and %d", f.cluster.changes, f.cluster.transfers, tc.changes, tc.transfers)
			}
		})
	}
}
