package overlay_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/cluster/discovery"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/raft"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/storage/storagetest"
)

// errRollback ends every contract transaction, so each case starts from the same
// committed rows.
var errRollback = errors.New("contract: roll the transaction back")

// backends are the stores the contract runs against, each opened empty.
var backends = []struct {
	name string
	open func(t *testing.T) storage.Storage
}{
	{"inmemory", func(*testing.T) storage.Storage { return inmemory.New() }},
	{"raft", openRaft},
}

// openRaft starts a single-node Raft cluster on loopback, whose one peer is the
// node itself, and returns its store once the node leads. The node dials no
// peer, so its transport runs plain TCP: TLS has no part in the contract.
func openRaft(t *testing.T) storage.Storage {
	t.Helper()
	hub := event.NewHub(zerolog.Nop())
	t.Cleanup(hub.Close)
	p, err := raft.New(raft.Config{
		NodeID:                 "contract",
		BindAddr:               "127.0.0.1:0",
		DataDir:                filepath.Join(t.TempDir(), "raft"),
		ElectionTimeout:        50 * time.Millisecond,
		HeartbeatTimeout:       50 * time.Millisecond,
		AllowInsecureTransport: true,
	}, inmemory.New(), hub, discovery.NewStatic([]cluster.Peer{{ID: "contract", Address: "127.0.0.1:0"}}), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown() })
	for deadline := time.Now().Add(2 * time.Second); !p.Leadership().AmLeader(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Raft node did not lead within two seconds")
		}
	}
	return p.Store()
}

// rywCase writes in a transaction and reads the write back in the same one. read
// names the storage.Storage method the check reads through.
type rywCase struct {
	name, read string
	write      func(t *testing.T, tx storage.Storage) error
	check      func(t *testing.T, tx storage.Storage) error
}

// TestOverlayReadsItsOwnWrites runs, on every backend, a transaction per case that
// writes and then reads its write back: point reads, index lookups and listings
// see its creates, updates, deletes and bulk deletes, and listings keep their order.
func TestOverlayReadsItsOwnWrites(t *testing.T) {
	cases := readYourWrites()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			if err := s.WithTx(t.Context(), func(tx storage.Storage) error { return seed(t, tx) }); err != nil {
				t.Fatalf("seed: %v", err)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					err := s.WithTx(t.Context(), func(tx storage.Storage) error {
						if err := tc.write(t, tx); err != nil {
							return fmt.Errorf("write: %w", err)
						}
						if err := tc.check(t, tx); err != nil {
							return fmt.Errorf("%s misses the write: %w", tc.read, err)
						}
						return errRollback
					})
					if !errors.Is(err, errRollback) {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

// TestContractCoversEveryRead fails when storage.Storage has a read method that no
// case of TestOverlayReadsItsOwnWrites reads through.
func TestContractCoversEveryRead(t *testing.T) {
	st := reflect.TypeFor[storage.Storage]()
	covered := map[string]bool{}
	for _, tc := range readYourWrites() {
		if _, ok := st.MethodByName(tc.read); !ok {
			t.Errorf("case %s reads through %s, which storage.Storage does not have", tc.name, tc.read)
		}
		covered[tc.read] = true
	}
	for i := range st.NumMethod() {
		name := st.Method(i).Name
		if (strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List")) && !covered[name] {
			t.Errorf("no case reads through %s", name)
		}
	}
}

func readYourWrites() []rywCase {
	return []rywCase{
		// Agents.
		{
			"GetAgent/created", "GetAgent",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertAgent(t.Context(), agent(t, "agent-a", 1, "prod"))
			},
			func(t *testing.T, tx storage.Storage) error {
				return present(tx.GetAgent(t.Context(), "agent-a"))
			},
		},
		{
			"GetAgent/updated", "GetAgent",
			func(t *testing.T, tx storage.Storage) error {
				a := agent(t, "agent-b", 3, "prod")
				a.SetOS("plan9")
				return tx.UpsertAgent(t.Context(), a)
			},
			func(t *testing.T, tx storage.Storage) error {
				a, err := tx.GetAgent(t.Context(), "agent-b")
				if err != nil {
					return err
				}
				return field("OS", a.OS(), "plan9")
			},
		},
		{
			"GetAgent/deleted", "GetAgent",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteAgent(t.Context(), "agent-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetAgent(t.Context(), "agent-b"))
			},
		},
		{
			"ListAgents/listing order", "ListAgents",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := agent(t, "agent-d", 1, "dev")
				d.SetOS("plan9")
				return errors.Join(
					tx.UpsertAgent(ctx, agent(t, "agent-a", 1, "dev")),
					tx.UpsertAgent(ctx, agent(t, "agent-c", 2, "dev")),
					tx.UpsertAgent(ctx, agent(t, "agent-e", 3, "dev")),
					tx.UpsertAgent(ctx, d),
					tx.DeleteAgent(ctx, "agent-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.AgentListResult, error) {
					return tx.ListAgents(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("agent-")...); err != nil {
					return err
				}
				return field("OS of agent-d", rows[4].OS(), "plan9")
			},
		},
		{
			"ListAgents/filtered", "ListAgents",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertAgent(ctx, agent(t, "agent-a", 1, "prod")),
					tx.UpsertAgent(ctx, agent(t, "agent-b", 3, "dev")),
					tx.UpsertAgent(ctx, agent(t, "agent-d", 1, "prod")),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.AgentListResult, error) {
					return tx.ListAgents(t.Context(), inmemory.NewAgentFilter().ByLabel("env", "prod"), o)
				})
				if err != nil {
					return err
				}
				return sameIDs(rows, "agent-f", "agent-a", "agent-d")
			},
		},

		// Agent credentials.
		{
			"GetAgentCredential/created", "GetAgentCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertAgentCredential(t.Context(), agentCredential(t, "agent-d", "token-d", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				return present(tx.GetAgentCredential(t.Context(), "agent-d"))
			},
		},
		{
			"GetAgentCredential/updated", "GetAgentCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertAgentCredential(t.Context(), agentCredential(t, "agent-b", "token-b2", 3))
			},
			func(t *testing.T, tx storage.Storage) error {
				c, err := tx.GetAgentCredential(t.Context(), "agent-b")
				if err != nil {
					return err
				}
				return field("token", c.Token(), "token-b2")
			},
		},
		{
			"GetAgentCredential/deleted", "GetAgentCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.DeleteAgentCredential(t.Context(), "agent-b")
			},
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetAgentCredential(t.Context(), "agent-b"))
			},
		},
		{
			"ListAgentCredentials/listing order", "ListAgentCredentials",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-a", "token-a", 1)),
					tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-c", "token-c", 2)),
					tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-e", "token-e", 3)),
					tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-g", "token-g", 2)),
					tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-b", "token-b2", 3)),
					tx.DeleteAgentCredential(ctx, "agent-g"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.AgentCredentialListResult, error) {
					return tx.ListAgentCredentials(t.Context(), o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, "agent-b", "agent-e", "agent-c", "agent-a"); err != nil {
					return err
				}
				return field("token of agent-b", rows[0].Token(), "token-b2")
			},
		},

		// Users.
		{
			"GetUser/created", "GetUser",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertUser(t.Context(), user(t, "user-a", "sub-a", 1))
			},
			func(t *testing.T, tx storage.Storage) error { return present(tx.GetUser(t.Context(), "user-a")) },
		},
		{
			"GetUser/deleted", "GetUser",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteUser(t.Context(), "user-b") },
			func(t *testing.T, tx storage.Storage) error { return missing(tx.GetUser(t.Context(), "user-b")) },
		},
		{
			"GetUserBySubject/created", "GetUserBySubject",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertUser(t.Context(), user(t, "user-a", "sub-a", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				u, err := tx.GetUserBySubject(t.Context(), "sub-a")
				return isRow(u, err, "user-a")
			},
		},
		{
			"GetUserBySubject/subject changed", "GetUserBySubject",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertUser(t.Context(), user(t, "user-b", "sub-z", 3))
			},
			func(t *testing.T, tx storage.Storage) error {
				if err := missing(tx.GetUserBySubject(t.Context(), "sub-b")); err != nil {
					return fmt.Errorf("old subject: %w", err)
				}
				u, err := tx.GetUserBySubject(t.Context(), "sub-z")
				return isRow(u, err, "user-b")
			},
		},
		{
			"GetUserBySubject/deleted", "GetUserBySubject",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteUser(t.Context(), "user-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetUserBySubject(t.Context(), "sub-b"))
			},
		},
		{
			"ListUsers/listing order", "ListUsers",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := user(t, "user-d", "sub-d", 1)
				d.SetName("renamed")
				return errors.Join(
					tx.UpsertUser(ctx, user(t, "user-a", "sub-a", 1)),
					tx.UpsertUser(ctx, user(t, "user-c", "sub-c", 2)),
					tx.UpsertUser(ctx, user(t, "user-e", "sub-e", 3)),
					tx.UpsertUser(ctx, d),
					tx.DeleteUser(ctx, "user-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.UserListResult, error) {
					return tx.ListUsers(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("user-")...); err != nil {
					return err
				}
				return field("name of user-d", rows[4].Name(), "renamed")
			},
		},

		{
			"ListUsers/filtered", "ListUsers",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				b := user(t, "user-b", "sub-b", 3)
				b.Disable()
				return errors.Join(
					tx.UpsertUser(ctx, user(t, "user-a", "sub-a", 1)),
					tx.UpsertUser(ctx, b),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.UserListResult, error) {
					return tx.ListUsers(t.Context(), inmemory.NewUserFilter().ByDisabled(false), o)
				})
				if err != nil {
					return err
				}
				return sameIDs(rows, "user-f", "user-a", "user-d")
			},
		},

		// Roles.
		{
			"GetRole/created", "GetRole",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertRole(t.Context(), role(t, "role-a", "admin", 1))
			},
			func(t *testing.T, tx storage.Storage) error { return present(tx.GetRole(t.Context(), "role-a")) },
		},
		{
			"GetRole/deleted", "GetRole",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteRole(t.Context(), "role-b") },
			func(t *testing.T, tx storage.Storage) error { return missing(tx.GetRole(t.Context(), "role-b")) },
		},
		{
			"GetRoles/created", "GetRoles",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertRole(t.Context(), role(t, "role-a", "admin", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				roles, err := tx.GetRoles(t.Context(), []string{"role-a", "role-b", "role-a"})
				if err != nil {
					return err
				}
				return sameIDs(roles, "role-a", "role-b", "role-a")
			},
		},
		{
			"GetRoles/deleted", "GetRoles",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteRole(t.Context(), "role-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetRoles(t.Context(), []string{"role-d", "role-b"}))
			},
		},
		{
			"GetRoleByName/renamed", "GetRoleByName",
			func(t *testing.T, tx storage.Storage) error {
				r := role(t, "role-b", "viewer", 3)
				if err := r.Rename("observer"); err != nil {
					return err
				}
				return tx.UpsertRole(t.Context(), r)
			},
			func(t *testing.T, tx storage.Storage) error {
				if err := missing(tx.GetRoleByName(t.Context(), "viewer")); err != nil {
					return fmt.Errorf("old name: %w", err)
				}
				r, err := tx.GetRoleByName(t.Context(), "observer")
				return isRow(r, err, "role-b")
			},
		},
		{
			"GetRoleByName/deleted", "GetRoleByName",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteRole(t.Context(), "role-d") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetRoleByName(t.Context(), "editor"))
			},
		},
		{
			"ListRoles/listing order", "ListRoles",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := role(t, "role-d", "editor", 1)
				if err := d.Rename("writer"); err != nil {
					return err
				}
				return errors.Join(
					tx.UpsertRole(ctx, role(t, "role-a", "admin", 1)),
					tx.UpsertRole(ctx, role(t, "role-c", "operator", 2)),
					tx.UpsertRole(ctx, role(t, "role-e", "owner", 3)),
					tx.UpsertRole(ctx, d),
					tx.DeleteRole(ctx, "role-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.RoleListResult, error) {
					return tx.ListRoles(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("role-")...); err != nil {
					return err
				}
				return field("name of role-d", rows[4].Name(), "writer")
			},
		},

		{
			"ListRoles/filtered", "ListRoles",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				b := role(t, "role-b", "viewer", 3)
				if err := b.Rename("observer"); err != nil {
					return err
				}
				return errors.Join(
					tx.UpsertRole(ctx, role(t, "role-a", "viewer", 1)),
					tx.UpsertRole(ctx, b),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.RoleListResult, error) {
					return tx.ListRoles(t.Context(), inmemory.NewRoleFilter().ByName("viewer"), o)
				})
				if err != nil {
					return err
				}
				return sameIDs(rows, "role-a")
			},
		},

		// Credentials.
		{
			"GetCredential/created", "GetCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertCredential(t.Context(), credential(t, "cred-a", "user-d", enum.APIKey, 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				return present(tx.GetCredential(t.Context(), "cred-a"))
			},
		},
		{
			"GetCredential/deleted", "GetCredential",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteCredential(t.Context(), "cred-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetCredential(t.Context(), "cred-b"))
			},
		},
		{
			"GetCredentialByUserAndAuth/created", "GetCredentialByUserAndAuth",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertCredential(t.Context(), credential(t, "cred-a", "user-d", enum.APIKey, 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				c, err := tx.GetCredentialByUserAndAuth(t.Context(), "user-d", enum.APIKey)
				return isRow(c, err, "cred-a")
			},
		},
		{
			"GetCredentialByUserAndAuth/auth changed", "GetCredentialByUserAndAuth",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertCredential(t.Context(), credential(t, "cred-g", "user-d", enum.APIKey, 2))
			},
			func(t *testing.T, tx storage.Storage) error {
				if err := missing(tx.GetCredentialByUserAndAuth(t.Context(), "user-d", enum.Password)); err != nil {
					return fmt.Errorf("old auth: %w", err)
				}
				c, err := tx.GetCredentialByUserAndAuth(t.Context(), "user-d", enum.APIKey)
				return isRow(c, err, "cred-g")
			},
		},
		{
			"GetCredentialByUserAndAuth/user changed", "GetCredentialByUserAndAuth",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertCredential(t.Context(), credential(t, "cred-d", "user-d", enum.APIKey, 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				if err := missing(tx.GetCredentialByUserAndAuth(t.Context(), "user-b", enum.APIKey)); err != nil {
					return fmt.Errorf("old user: %w", err)
				}
				c, err := tx.GetCredentialByUserAndAuth(t.Context(), "user-d", enum.APIKey)
				return isRow(c, err, "cred-d")
			},
		},
		{
			"GetCredentialByUserAndAuth/deleted", "GetCredentialByUserAndAuth",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteCredential(t.Context(), "cred-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetCredentialByUserAndAuth(t.Context(), "user-b", enum.Password))
			},
		},
		{
			"ListCredentialsByUser/listing order", "ListCredentialsByUser",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := credential(t, "cred-d", "user-b", enum.APIKey, 1)
				if err := d.SetSecret("key", "rotated"); err != nil {
					return err
				}
				return errors.Join(
					tx.UpsertCredential(ctx, credential(t, "cred-a", "user-b", "cert", 1)),
					tx.UpsertCredential(ctx, credential(t, "cred-c", "user-b", "oidc", 2)),
					tx.UpsertCredential(ctx, credential(t, "cred-e", "user-b", "saml", 3)),
					tx.UpsertCredential(ctx, d),
					tx.DeleteCredential(ctx, "cred-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := tx.ListCredentialsByUser(t.Context(), "user-b")
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("cred-")...); err != nil {
					return err
				}
				key, _ := rows[4].Secret("key")
				return field("secret of cred-d", key, "rotated")
			},
		},
		{
			"ListCredentialsByUser/moved", "ListCredentialsByUser",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertCredential(t.Context(), credential(t, "cred-d", "user-d", enum.APIKey, 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				was, err := tx.ListCredentialsByUser(t.Context(), "user-b")
				if err != nil {
					return err
				}
				if err := sameIDs(was, "cred-b", "cred-f"); err != nil {
					return fmt.Errorf("old user: %w", err)
				}
				now, err := tx.ListCredentialsByUser(t.Context(), "user-d")
				if err != nil {
					return err
				}
				return sameIDs(now, "cred-g", "cred-d")
			},
		},

		// Verifiers.
		{
			"GetVerifier/created", "GetVerifier",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertVerifier(t.Context(), verifier(t, "ver-d", "cred-d", 1))
			},
			func(t *testing.T, tx storage.Storage) error { return present(tx.GetVerifier(t.Context(), "ver-d")) },
		},
		{
			"GetVerifier/deleted", "GetVerifier",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteVerifier(t.Context(), "ver-b") },
			func(t *testing.T, tx storage.Storage) error { return missing(tx.GetVerifier(t.Context(), "ver-b")) },
		},
		{
			"GetVerifier/bulk deleted", "GetVerifier",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertVerifier(ctx, verifier(t, "ver-a", "cred-d", 1)),
					tx.DeleteVerifierByCredential(ctx, "cred-d"),
					tx.DeleteVerifierByCredential(ctx, "cred-b"),
					tx.UpsertVerifier(ctx, verifier(t, "ver-c", "cred-d", 2)),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					missing(tx.GetVerifier(ctx, "ver-a")),
					missing(tx.GetVerifier(ctx, "ver-b")),
					present(tx.GetVerifier(ctx, "ver-c")),
					present(tx.GetVerifier(ctx, "ver-g")),
				)
			},
		},
		{
			"GetVerifierByCredential/created", "GetVerifierByCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertVerifier(t.Context(), verifier(t, "ver-d", "cred-d", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				v, err := tx.GetVerifierByCredential(t.Context(), "cred-d")
				return isRow(v, err, "ver-d")
			},
		},
		{
			"GetVerifierByCredential/deleted", "GetVerifierByCredential",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteVerifier(t.Context(), "ver-b") },
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetVerifierByCredential(t.Context(), "cred-b"))
			},
		},
		{
			"GetVerifierByCredential/bulk deleted", "GetVerifierByCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.DeleteVerifierByCredential(t.Context(), "cred-b")
			},
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetVerifierByCredential(t.Context(), "cred-b"))
			},
		},

		{
			"GetVerifierByCredential/moved", "GetVerifierByCredential",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertVerifier(t.Context(), verifier(t, "ver-b", "cred-x", 3))
			},
			func(t *testing.T, tx storage.Storage) error {
				if err := missing(tx.GetVerifierByCredential(t.Context(), "cred-b")); err != nil {
					return fmt.Errorf("old credential: %w", err)
				}
				v, err := tx.GetVerifierByCredential(t.Context(), "cred-x")
				return isRow(v, err, "ver-b")
			},
		},
		{
			"GetVerifier/bulk delete of the old key after a move", "GetVerifier",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertVerifier(ctx, verifier(t, "ver-b", "cred-x", 3)),
					tx.DeleteVerifierByCredential(ctx, "cred-b"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				v, err := tx.GetVerifier(t.Context(), "ver-b")
				if err != nil {
					return fmt.Errorf("the moved verifier: %w", err)
				}
				return field("credential of ver-b", v.CredentialID(), "cred-x")
			},
		},
		{
			"GetVerifier/bulk delete of the new key after a move", "GetVerifier",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertVerifier(ctx, verifier(t, "ver-b", "cred-x", 3)),
					tx.DeleteVerifierByCredential(ctx, "cred-x"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetVerifier(t.Context(), "ver-b"))
			},
		},

		// Sessions.
		{
			"GetSession/created", "GetSession",
			func(t *testing.T, tx storage.Storage) error {
				return tx.CreateSession(t.Context(), session(t, "sess-a", "user-b", "cred-b", 1))
			},
			func(t *testing.T, tx storage.Storage) error { return present(tx.GetSession(t.Context(), "sess-a")) },
		},
		{
			"GetSession/rotated", "GetSession",
			func(t *testing.T, tx storage.Storage) error {
				return tx.RotateRefresh(t.Context(), "sess-b", []byte("rotated"), minute(3).Add(48*time.Hour))
			},
			func(t *testing.T, tx storage.Storage) error {
				s, err := tx.GetSession(t.Context(), "sess-b")
				if err != nil {
					return err
				}
				return field("refresh hash", string(s.RefreshHash()), "rotated")
			},
		},
		{
			"GetSession/revoked", "GetSession",
			func(t *testing.T, tx storage.Storage) error {
				return tx.RevokeSession(t.Context(), "sess-b", minute(4))
			},
			func(t *testing.T, tx storage.Storage) error {
				s, err := tx.GetSession(t.Context(), "sess-b")
				if err != nil {
					return err
				}
				if !s.RevokedAt().Equal(minute(4)) {
					return fmt.Errorf("revoked at %v, want the written %v", s.RevokedAt(), minute(4))
				}
				return nil
			},
		},
		{
			"GetSession/deleted", "GetSession",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteSession(t.Context(), "sess-b") },
			func(t *testing.T, tx storage.Storage) error { return missing(tx.GetSession(t.Context(), "sess-b")) },
		},
		{
			"GetSession/bulk deleted", "GetSession",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteSessionsByUser(t.Context(), "user-b") },
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(missing(tx.GetSession(ctx, "sess-b")), present(tx.GetSession(ctx, "sess-g")))
			},
		},
		{
			"ListSessionsByUser/listing order", "ListSessionsByUser",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.CreateSession(ctx, session(t, "sess-a", "user-b", "cred-b", 1)),
					tx.CreateSession(ctx, session(t, "sess-c", "user-b", "cred-b", 2)),
					tx.CreateSession(ctx, session(t, "sess-e", "user-b", "cred-b", 3)),
					tx.RotateRefresh(ctx, "sess-d", []byte("rotated"), minute(1).Add(48*time.Hour)),
					tx.DeleteSession(ctx, "sess-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := tx.ListSessionsByUser(t.Context(), "user-b")
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("sess-")...); err != nil {
					return err
				}
				return field("refresh hash of sess-d", string(rows[4].RefreshHash()), "rotated")
			},
		},
		{
			"ListSessionsByUser/moved", "ListSessionsByUser",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.DeleteSession(ctx, "sess-b"),
					tx.CreateSession(ctx, session(t, "sess-b", "user-d", "cred-g", 3)),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				lists := func() error {
					was, err := tx.ListSessionsByUser(ctx, "user-b")
					if err != nil {
						return err
					}
					if err := sameIDs(was, "sess-f", "sess-d"); err != nil {
						return fmt.Errorf("old user: %w", err)
					}
					now, err := tx.ListSessionsByUser(ctx, "user-d")
					if err != nil {
						return err
					}
					return sameIDs(now, "sess-b", "sess-g")
				}
				if err := lists(); err != nil {
					return err
				}
				// The old user's bulk delete must not reach the moved session.
				if err := tx.DeleteSessionsByUser(ctx, "user-b"); err != nil {
					return err
				}
				now, err := tx.ListSessionsByUser(ctx, "user-d")
				if err != nil {
					return err
				}
				if err := sameIDs(now, "sess-b", "sess-g"); err != nil {
					return fmt.Errorf("after the old user's bulk delete: %w", err)
				}
				return nil
			},
		},
		{
			"ListSessionsByUser/bulk deleted", "ListSessionsByUser",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.CreateSession(ctx, session(t, "sess-a", "user-b", "cred-b", 1)),
					tx.DeleteSessionsByUser(ctx, "user-b"),
					tx.CreateSession(ctx, session(t, "sess-c", "user-b", "cred-b", 2)),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				deleted, err := tx.ListSessionsByUser(ctx, "user-b")
				if err != nil {
					return err
				}
				if err := sameIDs(deleted, "sess-c"); err != nil {
					return fmt.Errorf("user-b: %w", err)
				}
				kept, err := tx.ListSessionsByUser(ctx, "user-d")
				if err != nil {
					return err
				}
				return sameIDs(kept, "sess-g")
			},
		},

		// Specs.
		{
			"GetSpec/created", "GetSpec",
			func(t *testing.T, tx storage.Storage) error { return tx.UpsertSpec(t.Context(), spec(t, "spec-a", 1)) },
			func(t *testing.T, tx storage.Storage) error { return present(tx.GetSpec(t.Context(), "spec-a")) },
		},
		{
			"GetSpec/deleted", "GetSpec",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteSpec(t.Context(), "spec-b") },
			func(t *testing.T, tx storage.Storage) error { return missing(tx.GetSpec(t.Context(), "spec-b")) },
		},
		{
			"ListSpecs/listing order", "ListSpecs",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := spec(t, "spec-d", 1)
				d.SetName("renamed")
				return errors.Join(
					tx.UpsertSpec(ctx, spec(t, "spec-a", 1)),
					tx.UpsertSpec(ctx, spec(t, "spec-c", 2)),
					tx.UpsertSpec(ctx, spec(t, "spec-e", 3)),
					tx.UpsertSpec(ctx, d),
					tx.DeleteSpec(ctx, "spec-f"),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.SpecListResult, error) {
					return tx.ListSpecs(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered("spec-")...); err != nil {
					return err
				}
				return field("name of spec-d", rows[4].Name(), "renamed")
			},
		},
		{
			"ListSpecs/filtered", "ListSpecs",
			func(t *testing.T, tx storage.Storage) error {
				b := spec(t, "spec-b", 3)
				b.SetName("renamed")
				return tx.UpsertSpec(t.Context(), b)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.SpecListResult, error) {
					return tx.ListSpecs(t.Context(), tx.BuildSpecFilter(storage.SpecQueryCriteria{Query: "job of"}), o)
				})
				if err != nil {
					return err
				}
				return sameIDs(rows, "spec-f", "spec-d")
			},
		},

		// Rollouts.
		{
			"GetRollout/created", "GetRollout",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertRollout(t.Context(), rollout(t, "spec-d", "agent-d", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				return present(tx.GetRollout(t.Context(), model.RolloutID("spec-d", "agent-d")))
			},
		},
		{
			"GetRollout/updated", "GetRollout",
			func(t *testing.T, tx storage.Storage) error {
				r := rollout(t, "spec-b", "agent-b", 3)
				r.MarkFailed("boom")
				return tx.UpsertRollout(t.Context(), r)
			},
			func(t *testing.T, tx storage.Storage) error {
				r, err := tx.GetRollout(t.Context(), model.RolloutID("spec-b", "agent-b"))
				if err != nil {
					return err
				}
				return field("error", r.Error(), "boom")
			},
		},
		{
			"GetRollout/deleted", "GetRollout",
			func(t *testing.T, tx storage.Storage) error {
				return tx.DeleteRollout(t.Context(), model.RolloutID("spec-b", "agent-b"))
			},
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetRollout(t.Context(), model.RolloutID("spec-b", "agent-b")))
			},
		},
		{
			"GetRollout/bulk deleted", "GetRollout",
			func(t *testing.T, tx storage.Storage) error { return tx.DeleteRolloutsBySpec(t.Context(), "spec-b") },
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					missing(tx.GetRollout(ctx, model.RolloutID("spec-b", "agent-b"))),
					present(tx.GetRollout(ctx, model.RolloutID("spec-d", "agent-b"))),
				)
			},
		},
		{
			"ListRollouts/listing order", "ListRollouts",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				d := rollout(t, "spec-b", "agent-d", 1)
				d.MarkFailed("boom")
				return errors.Join(
					tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-a", 1)),
					tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-c", 2)),
					tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-e", 3)),
					tx.UpsertRollout(ctx, d),
					tx.DeleteRollout(ctx, model.RolloutID("spec-b", "agent-f")),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				filter := tx.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: "spec-b"})
				rows, err := paged(func(o storage.ListOptions) (*storage.RolloutListResult, error) {
					return tx.ListRollouts(t.Context(), filter, o)
				})
				if err != nil {
					return err
				}
				if err := listingOrder(rows, lettered(model.RolloutID("spec-b", "agent-"))...); err != nil {
					return err
				}
				return field("error of agent-d", rows[4].Error(), "boom")
			},
		},
		{
			"ListRollouts/bulk deleted", "ListRollouts",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-a", 1)),
					tx.DeleteRolloutsBySpec(ctx, "spec-b"),
					tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-c", 2)),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.RolloutListResult, error) {
					return tx.ListRollouts(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				return sameIDs(rows, model.RolloutID("spec-b", "agent-c"), model.RolloutID("spec-d", "agent-b"))
			},
		},

		// Executions.
		{
			"GetExecution/created", "GetExecution",
			func(t *testing.T, tx storage.Storage) error {
				return tx.UpsertExecution(t.Context(), execution(t, "a", 1))
			},
			func(t *testing.T, tx storage.Storage) error {
				return present(tx.GetExecution(t.Context(), execution(t, "a", 1).ID()))
			},
		},
		{
			"GetExecution/rewritten", "GetExecution",
			func(t *testing.T, tx storage.Storage) error {
				p := execution(t, "b", 3).Params()
				p.Status = "failed"
				rewrite, err := model.NewExecutionRecord(p)
				if err != nil {
					return err
				}
				return tx.UpsertExecution(t.Context(), rewrite)
			},
			func(t *testing.T, tx storage.Storage) error {
				e, err := tx.GetExecution(t.Context(), execution(t, "b", 3).ID())
				if err != nil {
					return err
				}
				return field("status of the first result, which a rewrite keeps", e.Status(), "succeeded")
			},
		},
		{
			"GetExecution/deleted", "GetExecution",
			func(t *testing.T, tx storage.Storage) error {
				return tx.DeleteExecution(t.Context(), execution(t, "b", 3).ID())
			},
			func(t *testing.T, tx storage.Storage) error {
				return missing(tx.GetExecution(t.Context(), execution(t, "b", 3).ID()))
			},
		},
		{
			"ListExecutions/listing order", "ListExecutions",
			func(t *testing.T, tx storage.Storage) error {
				ctx := t.Context()
				return errors.Join(
					tx.UpsertExecution(ctx, execution(t, "a", 1)),
					tx.UpsertExecution(ctx, execution(t, "c", 2)),
					tx.UpsertExecution(ctx, execution(t, "e", 3)),
					tx.DeleteExecution(ctx, execution(t, "f", 2).ID()),
				)
			},
			func(t *testing.T, tx storage.Storage) error {
				rows, err := paged(func(o storage.ListOptions) (*storage.ExecutionListResult, error) {
					return tx.ListExecutions(t.Context(), nil, o)
				})
				if err != nil {
					return err
				}
				return listingOrder(rows, executionIDs(t, "b", "e", "c", "a", "d")...)
			},
		},
	}
}

// TestOverlayScansMatchListings compares each scan of a transaction with the
// matching listing drained page by page, while the transaction holds creates,
// updates, deletes and bulk-delete tombstones of the kind: the same rows, in the
// same order. A transaction handle that is not a storage.Scanner fails it, since
// the All* helpers would then drain its listings page by page.
func TestOverlayScansMatchListings(t *testing.T) {
	const rows = 600 // more than the 500 rows of a page
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := t.Context()
			s := b.open(t)
			if err := storagetest.Seed(ctx, s, storagetest.Fixture{Agents: rows, HistoryRows: rows}); err != nil {
				t.Fatal(err)
			}
			err := s.WithTx(ctx, func(tx storage.Storage) error {
				if err := fleetWrites(t, tx); err != nil {
					return fmt.Errorf("write: %w", err)
				}
				sc, ok := tx.(storage.Scanner)
				if !ok {
					return fmt.Errorf("the transaction handle %T is not a storage.Scanner", tx)
				}
				agents := func(region string) storage.AgentFilter {
					return inmemory.NewAgentFilter().ByLabel("region", region)
				}
				rollouts := func(spec string) storage.RolloutFilter {
					return tx.BuildRolloutFilter(storage.RolloutQueryCriteria{SpecID: spec})
				}
				specs := func(q string) storage.SpecFilter {
					return tx.BuildSpecFilter(storage.SpecQueryCriteria{Query: q})
				}
				if err := errors.Join(
					sameAsListing(ctx, "agents", sc.ScanAgents, tx.ListAgents, rows+3-1, agents("eu-west"), agents("pluto")),
					// Spec 3 lost every rollout to a bulk delete.
					sameAsListing(ctx, "rollouts", sc.ScanRollouts, tx.ListRollouts, rows*storagetest.RolloutsPerAgent+3-1-90,
						rollouts(storagetest.SpecID(0)), rollouts(storagetest.SpecID(3))),
					sameAsListing(ctx, "specs", sc.ScanSpecs, tx.ListSpecs, storagetest.SpecCount+1-1,
						specs("job-1"), specs("no-such-job")),
					sameAsListing(ctx, "executions", sc.ScanExecutions, tx.ListExecutions, rows+2-1,
						tx.BuildExecutionFilter(storage.ExecutionQueryCriteria{Statuses: []string{"failed"}}),
						tx.BuildExecutionFilter(storage.ExecutionQueryCriteria{AgentID: "no-such-agent"})),
				); err != nil {
					return err
				}
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatal(err)
			}
		})
	}
}

// fleetWrites writes, over the storagetest fleet, every kind of change a scan must
// honour: rows created tied with committed ones, before and after them in ID order;
// committed rows updated into and out of a filter; and rows deleted one by one and
// in bulk.
func fleetWrites(t *testing.T, tx storage.Storage) error {
	ctx := t.Context()
	fleetAgent := func(id string, created time.Time, region string) *model.Agent {
		a := agent(t, id, 0, "prod")
		a.SetLabels(map[string]string{"env": "prod", "region": region})
		a.SetCreatedAt(created)
		return a
	}
	relabelled := func(i int, region string) (*model.Agent, error) {
		a, err := tx.GetAgent(ctx, storagetest.AgentID(i))
		if err != nil {
			return nil, err
		}
		a.LabelAdd("region", region)
		return a, nil
	}
	intoFilter, err := relabelled(30, "eu-west")
	if err != nil {
		return err
	}
	outOfFilter, err := relabelled(40, "mars")
	if err != nil {
		return err
	}
	failed, err := tx.GetRollout(ctx, model.RolloutID(storagetest.SpecID(1), storagetest.AgentID(1)))
	if err != nil {
		return err
	}
	failed.MarkFailed("boom")
	renamed, err := tx.GetSpec(ctx, storagetest.SpecID(12))
	if err != nil {
		return err
	}
	renamed.SetName("renamed")
	newSpec := spec(t, storagetest.SpecID(storagetest.SpecCount), 0)
	newSpec.SetName("job-1x")
	newSpec.SetCreatedAt(storagetest.Epoch)
	newest, err := tx.ListExecutions(ctx, nil, storage.ListOptions{Limit: 3})
	if err != nil {
		return err
	}
	tied := storagetest.Epoch.Add(-10 * time.Minute) // the creation time of agent 10
	created := []*model.Agent{
		fleetAgent("agent-00009x", tied, "eu-west"), // before agent 10 in ID order
		fleetAgent("agent-00600", tied, "us-east"),  // after it
		fleetAgent("agent-00601", storagetest.Epoch.Add(time.Minute), "eu-east"),
	}
	writes := []error{
		tx.UpsertAgent(ctx, intoFilter),
		tx.UpsertAgent(ctx, outOfFilter),
		tx.DeleteAgent(ctx, storagetest.AgentID(50)),
		tx.UpsertRollout(ctx, failed),
		tx.DeleteRollout(ctx, model.RolloutID(storagetest.SpecID(0), storagetest.AgentID(0))),
		tx.DeleteRolloutsBySpec(ctx, storagetest.SpecID(3)),
		tx.UpsertSpec(ctx, renamed),
		tx.UpsertSpec(ctx, newSpec),
		tx.DeleteSpec(ctx, storagetest.SpecID(7)),
		tx.UpsertExecution(ctx, fleetExecution(t, "new-1", storagetest.Epoch.Add(100*time.Second))),
		tx.UpsertExecution(ctx, fleetExecution(t, "new-2", storagetest.Epoch.Add(250500*time.Millisecond))),
		tx.DeleteExecution(ctx, newest.Items[2].ID()),
	}
	for _, a := range created {
		r := rollout(t, storagetest.SpecID(0), a.ID(), 0)
		r.SetCreatedAt(storagetest.Epoch)
		writes = append(writes, tx.UpsertAgent(ctx, a), tx.UpsertRollout(ctx, r))
	}
	return errors.Join(writes...)
}

func fleetExecution(t *testing.T, uid string, captured time.Time) *model.ExecutionRecord {
	t.Helper()
	code := int32(1)
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
		AgentID: storagetest.AgentID(1), AgentName: "edge", TaskName: "job-01", TaskUID: uid,
		Generation: 1, Attempt: 1, Status: "failed", ExitCode: &code, Error: "exit status 1", CapturedAt: captured,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// sameAsListing compares a scan with its listing drained by storage.CollectPages
// for every row, which must be all rows, and for each filter, the first of which
// must match some rows and the last none.
func sameAsListing[T, F any](ctx context.Context, kind string, scan func(context.Context, F) ([]T, error), list func(context.Context, F, storage.ListOptions) (*storage.ListResult[T], error), all int, some, none F) error {
	var every F
	for _, tc := range []struct {
		name   string
		filter F
		count  func(int) bool
	}{
		{"every row", every, func(n int) bool { return n == all }},
		{"some rows", some, func(n int) bool { return n > 0 && n < all }},
		{"no row", none, func(n int) bool { return n == 0 }},
	} {
		want, err := storage.CollectPages(ctx, func(o storage.ListOptions) (*storage.ListResult[T], error) {
			return list(ctx, tc.filter, o)
		})
		if err != nil {
			return fmt.Errorf("%s, %s: list: %w", kind, tc.name, err)
		}
		got, err := scan(ctx, tc.filter)
		if err != nil {
			return fmt.Errorf("%s, %s: scan: %w", kind, tc.name, err)
		}
		if !tc.count(len(got)) {
			return fmt.Errorf("%s, %s: the scan returned %d rows of %d", kind, tc.name, len(got), all)
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%s, %s: the scan (%d rows) differs from the listing (%d rows)", kind, tc.name, len(got), len(want))
		}
	}
	return nil
}

// seed commits the rows every case starts from. Each kind holds rows b, d and f,
// created at minutes 3, 1 and 2; the cases write rows a, c and e, created at
// minutes 1, 2 and 3, so that each is tied with a committed row, once before it in
// ID order and once after.
func seed(t *testing.T, tx storage.Storage) error {
	ctx := t.Context()
	return errors.Join(
		tx.UpsertAgent(ctx, agent(t, "agent-b", 3, "prod")),
		tx.UpsertAgent(ctx, agent(t, "agent-d", 1, "dev")),
		tx.UpsertAgent(ctx, agent(t, "agent-f", 2, "prod")),
		tx.UpsertAgentCredential(ctx, agentCredential(t, "agent-b", "token-b", 3)),
		tx.UpsertUser(ctx, user(t, "user-b", "sub-b", 3)),
		tx.UpsertUser(ctx, user(t, "user-d", "sub-d", 1)),
		tx.UpsertUser(ctx, user(t, "user-f", "sub-f", 2)),
		tx.UpsertRole(ctx, role(t, "role-b", "viewer", 3)),
		tx.UpsertRole(ctx, role(t, "role-d", "editor", 1)),
		tx.UpsertRole(ctx, role(t, "role-f", "auditor", 2)),
		tx.UpsertCredential(ctx, credential(t, "cred-b", "user-b", enum.Password, 3)),
		tx.UpsertCredential(ctx, credential(t, "cred-d", "user-b", enum.APIKey, 1)),
		tx.UpsertCredential(ctx, credential(t, "cred-f", "user-b", "token", 2)),
		tx.UpsertCredential(ctx, credential(t, "cred-g", "user-d", enum.Password, 2)),
		tx.UpsertVerifier(ctx, verifier(t, "ver-b", "cred-b", 3)),
		tx.UpsertVerifier(ctx, verifier(t, "ver-g", "cred-g", 2)),
		tx.CreateSession(ctx, session(t, "sess-b", "user-b", "cred-b", 3)),
		tx.CreateSession(ctx, session(t, "sess-d", "user-b", "cred-b", 1)),
		tx.CreateSession(ctx, session(t, "sess-f", "user-b", "cred-b", 2)),
		tx.CreateSession(ctx, session(t, "sess-g", "user-d", "cred-g", 2)),
		tx.UpsertSpec(ctx, spec(t, "spec-b", 3)),
		tx.UpsertSpec(ctx, spec(t, "spec-d", 1)),
		tx.UpsertSpec(ctx, spec(t, "spec-f", 2)),
		tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-b", 3)),
		tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-d", 1)),
		tx.UpsertRollout(ctx, rollout(t, "spec-b", "agent-f", 2)),
		tx.UpsertRollout(ctx, rollout(t, "spec-d", "agent-b", 2)),
		tx.UpsertExecution(ctx, execution(t, "b", 3)),
		tx.UpsertExecution(ctx, execution(t, "d", 1)),
		tx.UpsertExecution(ctx, execution(t, "f", 2)),
	)
}

// lettered names the rows b, e, c, a and d of a kind: the listing order once a case
// has written a, c and e, updated d and deleted f.
func lettered(prefix string) []string {
	return []string{prefix + "b", prefix + "e", prefix + "c", prefix + "a", prefix + "d"}
}

type identified interface{ ID() string }

// sameIDs is nil when rows are exactly the rows want names, in that order.
func sameIDs[T identified](rows []T, want ...string) error {
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.ID()
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("got %v, want %v", got, want)
	}
	return nil
}

// listingOrder is sameIDs for a listing-order case. Such a case pins both keys of
// the order only if want is not its rows in ID order and two of its rows share a
// creation time, and it fails when that is not so.
func listingOrder[T interface {
	identified
	CreatedAt() time.Time
}](rows []T, want ...string) error {
	if slices.IsSorted(want) {
		return fmt.Errorf("the case pins nothing: %v is the order of the IDs", want)
	}
	if err := sameIDs(rows, want...); err != nil {
		return err
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt().Equal(rows[i-1].CreatedAt()) {
			return nil
		}
	}
	return fmt.Errorf("the case pins nothing: no two of %v share a creation time", want)
}

// paged drains a listing two rows at a time, so that its cursors fall between
// committed rows, written rows and rows tied with each other.
func paged[T any](list func(storage.ListOptions) (*storage.ListResult[T], error)) ([]T, error) {
	var rows []T
	cursor := ""
	for range 10 {
		page, err := list(storage.ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Items...)
		if page.NextCursor == "" {
			return rows, nil
		}
		cursor = page.NextCursor
	}
	return nil, errors.New("the listing did not end within ten pages")
}

// present is nil when a read found its row.
func present[T any](_ T, err error) error { return err }

// missing is nil when a read found no row.
func missing[T any](_ T, err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil
	case err == nil:
		return errors.New("found a row, want ErrNotFound")
	default:
		return fmt.Errorf("%w, want ErrNotFound", err)
	}
}

// isRow is nil when a read found the row id.
func isRow[T identified](row T, err error, id string) error {
	if err != nil {
		return err
	}
	if row.ID() != id {
		return fmt.Errorf("found %s, want %s", row.ID(), id)
	}
	return nil
}

func field(name, got, want string) error {
	if got != want {
		return fmt.Errorf("%s = %q, want the written %q", name, got, want)
	}
	return nil
}

var epoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// minute is a creation time of the contract, m minutes after its epoch.
func minute(m int) time.Time { return epoch.Add(time.Duration(m) * time.Minute) }

func agent(t *testing.T, id string, created int, env string) *model.Agent {
	t.Helper()
	a, err := model.NewAgent(id, "agent "+id, "http://"+id+":8085")
	if err != nil {
		t.Fatal(err)
	}
	a.SetOS("linux")
	a.SetLabels(map[string]string{"env": env})
	a.SetCreatedAt(minute(created))
	a.SetUpdatedAt(minute(created))
	return a
}

func agentCredential(t *testing.T, agentID, token string, created int) *model.AgentCredential {
	t.Helper()
	c, err := model.NewStoredAgentCredential(agentID, token)
	if err != nil {
		t.Fatal(err)
	}
	c.SetCreatedAt(minute(created))
	c.SetUpdatedAt(minute(created))
	return c
}

func user(t *testing.T, id, subject string, created int) *model.User {
	t.Helper()
	u, err := model.NewUser(id, subject)
	if err != nil {
		t.Fatal(err)
	}
	u.SetCreatedAt(minute(created))
	u.SetUpdatedAt(minute(created))
	return u
}

func role(t *testing.T, id, name string, created int) *model.Role {
	t.Helper()
	r, err := model.NewRole(id, name)
	if err != nil {
		t.Fatal(err)
	}
	r.SetCreatedAt(minute(created))
	r.SetUpdatedAt(minute(created))
	return r
}

func credential(t *testing.T, id, userID string, auth enum.Auth, created int) *model.Credential {
	t.Helper()
	c, err := model.NewCredential(id, userID, auth)
	if err != nil {
		t.Fatal(err)
	}
	c.SetCreatedAt(minute(created))
	c.SetUpdatedAt(minute(created))
	return c
}

func verifier(t *testing.T, id, credentialID string, created int) *model.Verifier {
	t.Helper()
	v, err := model.NewVerifier(id, credentialID, enum.Password)
	if err != nil {
		t.Fatal(err)
	}
	v.SetCreatedAt(minute(created))
	v.SetUpdatedAt(minute(created))
	return v
}

func session(t *testing.T, id, userID, credentialID string, created int) *model.Session {
	t.Helper()
	s, err := model.NewSession(id, userID, credentialID, enum.Password, []byte("hash of "+id), minute(created).Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreatedAt(minute(created))
	s.SetUpdatedAt(minute(created))
	return s
}

func spec(t *testing.T, id string, created int) *model.Spec {
	t.Helper()
	s, err := model.NewSpec(id, "job of "+id, "slot of "+id)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreatedAt(minute(created))
	s.SetUpdatedAt(minute(created))
	return s
}

func rollout(t *testing.T, specID, agentID string, created int) *model.Rollout {
	t.Helper()
	r, err := model.NewRollout(specID, agentID, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.SetCreatedAt(minute(created))
	r.SetUpdatedAt(minute(created))
	return r
}

// execution builds the record named by letter, a to f. A record's ID is a digest of
// its run identity, so the letters go to six identities in the order of their IDs:
// the records sort by ID as their letters do.
func execution(t *testing.T, letter string, created int) *model.ExecutionRecord {
	t.Helper()
	rank := strings.Index("abcdef", letter)
	if rank < 0 || len(letter) != 1 {
		t.Fatalf("no execution record %q", letter)
	}
	byID := make([]*model.ExecutionRecord, 6)
	for i := range byID {
		byID[i] = run(t, fmt.Sprintf("uid-%d", i), created)
	}
	sort.Slice(byID, func(i, j int) bool { return byID[i].ID() < byID[j].ID() })
	return byID[rank]
}

func executionIDs(t *testing.T, letters ...string) []string {
	t.Helper()
	out := make([]string, len(letters))
	for i, l := range letters {
		out[i] = execution(t, l, 0).ID()
	}
	return out
}

func run(t *testing.T, uid string, created int) *model.ExecutionRecord {
	t.Helper()
	code := int32(0)
	e, err := model.NewExecutionRecord(model.ExecutionRecordParams{
		AgentID: "agent-b", AgentName: "agent b", TaskName: "task", TaskUID: uid,
		Generation: 1, Attempt: 1, Status: "succeeded", ExitCode: &code, CapturedAt: minute(created),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
