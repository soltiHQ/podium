package handler

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	taskv1 "github.com/soltiHQ/control-plane/api/gen/solti/task/v1"
	"github.com/soltiHQ/control-plane/domain/enum"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/auth/identity"
	"github.com/soltiHQ/control-plane/internal/auth/kit"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/event"
	"github.com/soltiHQ/control-plane/internal/loghub"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/history"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/spec"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"
	"github.com/soltiHQ/control-plane/internal/uikit/routepath"
)

// apiFixture is the production API wiring over an in-memory store that holds
// the built-in roles. Requests carry the identity the auth middleware would
// attach after AuthorizeAccess.
type apiFixture struct {
	store *inmemory.Store
	hub   *event.Hub
	mux   *http.ServeMux
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	ctx := context.Background()
	logger := zerolog.Nop()
	store := inmemory.New()
	for _, br := range enum.BuiltinRoles {
		role, err := model.NewRole(br.ID, br.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range br.Permissions {
			if err := role.PermissionAdd(p); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.UpsertRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := history.New(store, history.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(logger)
	t.Cleanup(hub.Close)
	pool := proxy.NewPool(nil, true)
	t.Cleanup(func() { _ = pool.Close() })
	logs := loghub.NewIdentity(func(context.Context, string, string, string) (<-chan *taskv1.StreamTaskLogsResponse, error) {
		return nil, storage.ErrNotFound
	}, loghub.Options{})
	t.Cleanup(logs.Close)
	streams := middleware.NewStreamLimiter(0, nil)
	api := NewAPI(logger,
		user.New(store, logger),
		access.New(kit.New(store, auth.Config{JWTSecret: strings.Repeat("k", auth.MinJWTSecretLen)}), store, logger),
		session.New(store, logger),
		credential.New(store, logger),
		agent.New(store, logger),
		spec.New(store, logger),
		archive, pool, hub, logs, streams,
	)
	mux := http.NewServeMux()
	api.Routes(mux, requireIdentity, nil)
	// app.buildMainHandler registers the event stream next to the API routes.
	route.HandleFunc(mux, routepath.ApiEventStream, hub.SSEHandler(), requireIdentity, middleware.StreamLimit(streams))
	return &apiFixture{store: store, hub: hub, mux: mux}
}

// requireIdentity stands in for middleware.Auth: a request without an
// identity is unauthenticated.
func requireIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := transportctx.Identity(r.Context()); !ok {
			response.Unauthorized(w, r, httpctx.ModeFromRequest(r))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// user stores an enabled user with the given roles and returns the identity
// request authorization resolves for it.
func (f *apiFixture) user(t *testing.T, id string, roleIDs ...string) *identity.Identity {
	t.Helper()
	ctx := context.Background()
	u, err := model.NewUser(id, id)
	if err != nil {
		t.Fatal(err)
	}
	u.SetName(id)
	u.SetRoleIDs(roleIDs)
	if err := f.store.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	perms, err := rbac.NewResolver(f.store).ResolveUserPermissions(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	return &identity.Identity{UserID: id, Subject: id, Name: id, Permissions: perms}
}

func (f *apiFixture) session(t *testing.T, id, userID string) {
	t.Helper()
	s, err := model.NewSession(id, userID, "cred-"+userID, enum.Password, make([]byte, 32), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func (f *apiFixture) spec(t *testing.T, id string) {
	t.Helper()
	ts, err := model.NewSpec(id, "Spec "+id, "slot-"+id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertSpec(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
}

// do sends a JSON-mode request as actor; a nil actor is unauthenticated. A
// body is sent as application/json.
func (f *apiFixture) do(method, target, body string, actor *identity.Identity) *httptest.ResponseRecorder {
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	return f.send(method, target, contentType, body, actor)
}

// send is do with the given Content-Type; an empty one sends none.
func (f *apiFixture) send(method, target, contentType, body string, actor *identity.Identity) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if actor != nil {
		r = r.WithContext(transportctx.WithIdentity(r.Context(), actor))
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// render sends an HTMX request and returns the HTML fragment.
func (f *apiFixture) render(t *testing.T, target string, actor *identity.Identity) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("HX-Request", "true")
	ctx := httpctx.WithResponder(r.Context(), responder.NewHTML())
	r = r.WithContext(transportctx.WithIdentity(ctx, actor))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// streamRecorder lets an SSE handler clear its write deadline.
type streamRecorder struct{ *httptest.ResponseRecorder }

func (streamRecorder) SetWriteDeadline(time.Time) error { return nil }

func allExcept(missing enum.Permission) []enum.Permission {
	return slices.DeleteFunc(slices.Clone(enum.All), func(p enum.Permission) bool { return p == missing })
}

// routePermission is one row of the matrix: a route as registered ({id} and
// {taskID} are path wildcards), the permissions it requires (all of them; none
// means any authenticated user) and a request that reaches its handler.
type routePermission struct {
	method  string
	pattern string
	id      string
	query   string
	body    string
	perms   []enum.Permission
	stream  bool
}

func (rp routePermission) target() string {
	return strings.NewReplacer("{id}", rp.id, "{taskID}", "task-1").Replace(rp.pattern) + rp.query
}

// routePermissionMatrix lists every /api/v1 route of the main listener. A new
// route needs a row here: TestRoutePermissionMatrix fails for a registered
// route without one, and for a row that matches no registered route.
var routePermissionMatrix = []routePermission{
	{method: http.MethodGet, pattern: "/api/v1/users", perms: []enum.Permission{enum.UsersGet}},
	{method: http.MethodPost, pattern: "/api/v1/users", body: `{"subject":"new-user"}`, perms: []enum.Permission{enum.UsersAdd}},
	{method: http.MethodGet, pattern: "/api/v1/users/{id}", id: "target", perms: []enum.Permission{enum.UsersGet}},
	{method: http.MethodPut, pattern: "/api/v1/users/{id}", id: "target", body: `{"name":"Renamed"}`, perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodDelete, pattern: "/api/v1/users/{id}", id: "target", perms: []enum.Permission{enum.UsersDelete}},
	{method: http.MethodGet, pattern: "/api/v1/users/{id}/sessions", id: "target", perms: []enum.Permission{enum.UsersGet}},
	{method: http.MethodPost, pattern: "/api/v1/users/{id}/disable", id: "target", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodPost, pattern: "/api/v1/users/{id}/enable", id: "target", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodPost, pattern: "/api/v1/users/{id}/password", id: "target", body: `{"password":"route-matrix-password"}`, perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodPost, pattern: "/api/v1/session/{id}/revoke", id: "target-session", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodGet, pattern: "/api/v1/agents", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodGet, pattern: "/api/v1/agents/{id}", id: "agent-1", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodPut, pattern: "/api/v1/agents/{id}/labels", id: "agent-1", body: `{}`, perms: []enum.Permission{enum.AgentsEdit}},
	{method: http.MethodGet, pattern: "/api/v1/agents/{id}/tasks", id: "agent-1", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodGet, pattern: "/api/v1/agents/{id}/tasks/{taskID}", id: "agent-1", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodPost, pattern: "/api/v1/agents/{id}/tasks/{taskID}/cancel", id: "agent-1", perms: []enum.Permission{enum.AgentsEdit}},
	{method: http.MethodGet, pattern: "/api/v1/agents/{id}/tasks/{taskID}/logs/stream", id: "agent-1", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodGet, pattern: "/api/v1/history", perms: []enum.Permission{enum.AgentsGet}},
	{method: http.MethodGet, pattern: "/api/v1/specs", perms: []enum.Permission{enum.SpecsGet}},
	{method: http.MethodPost, pattern: "/api/v1/specs", body: `{"name":"New spec","slot":"new-slot"}`, perms: []enum.Permission{enum.SpecsAdd}},
	{method: http.MethodGet, pattern: "/api/v1/specs/{id}", id: "spec-1", perms: []enum.Permission{enum.SpecsGet}},
	{method: http.MethodPut, pattern: "/api/v1/specs/{id}", id: "spec-1", body: `{"version":1,"name":"Renamed"}`, perms: []enum.Permission{enum.SpecsEdit}},
	{method: http.MethodDelete, pattern: "/api/v1/specs/{id}", id: "spec-1", perms: []enum.Permission{enum.SpecsDelete}},
	{method: http.MethodDelete, pattern: "/api/v1/specs/{id}/force", id: "spec-1", perms: []enum.Permission{enum.SpecsDelete}},
	{method: http.MethodPost, pattern: "/api/v1/specs/{id}/deploy", id: "spec-1", perms: []enum.Permission{enum.SpecsDeploy}},
	{method: http.MethodGet, pattern: "/api/v1/specs/{id}/sync", id: "spec-1", perms: []enum.Permission{enum.SpecsGet}},
	{method: http.MethodGet, pattern: "/api/v1/specs/{id}/placement", id: "spec-1", perms: []enum.Permission{enum.SpecsGet, enum.AgentsGet}},
	{method: http.MethodGet, pattern: "/api/v1/dashboard"},
	{method: http.MethodDelete, pattern: "/api/v1/dashboard/issues", query: "?kind=agent_inactive&entity=agent-1", perms: []enum.Permission{enum.AgentsEdit}},
	{method: http.MethodDelete, pattern: "/api/v1/dashboard/issues", query: "?kind=sync_failed&entity=spec-1", perms: []enum.Permission{enum.SpecsEdit}},
	{method: http.MethodDelete, pattern: "/api/v1/dashboard/issues", query: "?kind=rate_limited&entity=alice", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodDelete, pattern: "/api/v1/dashboard/issues", query: "?kind=not_a_kind&entity=x", perms: []enum.Permission{enum.AgentsEdit, enum.SpecsEdit, enum.UsersEdit}},
	{method: http.MethodGet, pattern: "/api/v1/permissions", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodGet, pattern: "/api/v1/roles", perms: []enum.Permission{enum.UsersEdit}},
	{method: http.MethodGet, pattern: "/api/v1/events/stream", stream: true},
}

// unlistedRoutes are /api/v1 routes outside the user permission model.
var unlistedRoutes = map[string]string{
	"/api/v1/discovery/sync": "discovery listener: agents authenticate with bearer tokens (see discovery tests)",
}

func TestRoutePermissionMatrix(t *testing.T) {
	t.Run("rows match the registered routes", func(t *testing.T) {
		registered := registeredAPIRoutes(t)
		listed := make(map[string]bool)
		for _, rp := range routePermissionMatrix {
			listed[rp.method+" "+rp.pattern] = true
		}
		for key, pos := range registered {
			method, path, _ := strings.Cut(key, " ")
			if _, ok := unlistedRoutes[path]; ok {
				continue
			}
			found := listed[key]
			if method == "*" {
				for row := range listed {
					found = found || strings.HasSuffix(row, " "+path)
				}
			}
			if !found {
				t.Errorf("%s: route %s has no row in routePermissionMatrix", pos, key)
			}
		}
		for row := range listed {
			_, path, _ := strings.Cut(row, " ")
			if _, ok := registered[row]; !ok {
				if _, ok := registered["* "+path]; !ok {
					t.Errorf("matrix row %s matches no registered route", row)
				}
			}
		}
	})

	for _, rp := range routePermissionMatrix {
		t.Run(rp.method+" "+rp.target(), func(t *testing.T) {
			call := func(actor *identity.Identity) int {
				f := newAPIFixture(t)
				f.user(t, "target")
				f.session(t, "target-session", "target")
				f.spec(t, "spec-1")
				f.hub.Record(event.AgentInactive, event.Payload{ID: "agent-1"})
				f.hub.Record(event.SyncFailed, event.Payload{ID: "spec-1"})
				f.hub.Record(event.RateLimited, event.Payload{ID: "alice"})
				r := httptest.NewRequest(rp.method, rp.target(), strings.NewReader(rp.body))
				if rp.body != "" {
					r.Header.Set("Content-Type", "application/json")
				}
				ctx := r.Context()
				if rp.stream {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if actor != nil {
					ctx = transportctx.WithIdentity(ctx, actor)
				}
				w := streamRecorder{httptest.NewRecorder()}
				f.mux.ServeHTTP(w, r.WithContext(ctx))
				return w.Code
			}

			if got := call(nil); got != http.StatusUnauthorized {
				t.Errorf("unauthenticated: %d, want 401", got)
			}
			got := call(&identity.Identity{UserID: "probe"})
			switch {
			case len(rp.perms) > 0 && got != http.StatusForbidden:
				t.Errorf("no permissions: %d, want 403", got)
			case len(rp.perms) == 0 && (got == http.StatusUnauthorized || got == http.StatusForbidden):
				t.Errorf("authenticated without permissions: %d, want access", got)
			}
			for _, p := range rp.perms {
				if got := call(&identity.Identity{UserID: "probe", Permissions: allExcept(p)}); got != http.StatusForbidden {
					t.Errorf("every permission but %s: %d, want 403", p, got)
				}
			}
			if got := call(&identity.Identity{UserID: "probe", Permissions: rp.perms}); got == http.StatusUnauthorized || got == http.StatusForbidden {
				t.Errorf("exactly %v: %d, want access", rp.perms, got)
			}
		})
	}
}

func TestSpecDeleteRequiresSpecsDelete(t *testing.T) {
	for _, target := range []string{"/api/v1/specs/spec-1", "/api/v1/specs/spec-1/force"} {
		t.Run(target, func(t *testing.T) {
			f := newAPIFixture(t)
			f.spec(t, "spec-1")
			editor := f.user(t, "spec-editor", enum.RoleSpecEditorID)
			if w := f.do(http.MethodDelete, target, "", editor); w.Code != http.StatusForbidden {
				t.Fatalf("specEditor: %d %s, want 403", w.Code, w.Body.String())
			}
			if _, err := f.store.GetSpec(context.Background(), "spec-1"); err != nil {
				t.Fatalf("refused delete removed the spec: %v", err)
			}
			admin := f.user(t, "spec-admin", enum.RoleSpecAdminID)
			if w := f.do(http.MethodDelete, target, "", admin); w.Code != http.StatusNoContent {
				t.Fatalf("specAdmin: %d %s, want 204", w.Code, w.Body.String())
			}
			if _, err := f.store.GetSpec(context.Background(), "spec-1"); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("specAdmin delete kept the spec: %v", err)
			}
		})
	}
}

// registeredAPIRoutes reads the /api/v1 registrations of this package and of
// internal/app/build.go and returns "METHOD /pattern" keys with their source
// position. route.Endpoint and route.Subroute tables and r.Method checks in
// the registered API method give the methods; "*" means the scan found none.
func registeredAPIRoutes(t *testing.T) map[string]token.Position {
	t.Helper()
	consts := routepathConstants(t)
	fset := token.NewFileSet()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var pkg []*ast.File
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg = append(pkg, f)
	}
	build, err := parser.ParseFile(fset, filepath.Join("..", "app", "build.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	tables := apiRouteTables(t, fset, pkg)
	out := make(map[string]token.Position)
	scan := func(f *ast.File, apiMethods bool) {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (fn.Sel.Name != "HandleFunc" && fn.Sel.Name != "Handle") {
				return true
			}
			args := call.Args
			if pkgName, ok := fn.X.(*ast.Ident); ok && pkgName.Name == "route" {
				args = args[1:] // route.HandleFunc(mux, pattern, handler, mws...)
			}
			if len(args) < 2 {
				return true
			}
			pos := fset.Position(call.Pos())
			pattern, ok := resolvePattern(args[0], consts)
			if !ok {
				t.Errorf("%s: cannot resolve the route pattern; extend registeredAPIRoutes", pos)
				return true
			}
			method, path, hasMethod := strings.Cut(pattern, " ")
			if !hasMethod {
				method, path = "*", pattern
			}
			if !strings.HasPrefix(path, "/api/v1/") {
				return true
			}
			var decls []routeDecl
			if handler, ok := args[1].(*ast.SelectorExpr); ok && apiMethods {
				decls = tables[handler.Sel.Name]
			}
			if len(decls) == 0 {
				out[method+" "+path] = pos
			}
			for _, d := range decls {
				p := path
				if d.sub {
					p += "{id}"
					if d.action != "" {
						p += "/" + d.action
					}
				}
				out[d.method+" "+p] = pos
			}
			return true
		})
	}
	for _, f := range pkg {
		scan(f, true)
	}
	scan(build, false)
	return out
}

// routeDecl is one method a handler serves: a route.Endpoint, a
// route.Subroute (sub, with its action) or an r.Method check.
type routeDecl struct {
	method string
	action string
	sub    bool
}

func apiRouteTables(t *testing.T, fset *token.FileSet, files []*ast.File) map[string][]routeDecl {
	t.Helper()
	tables := make(map[string][]routeDecl)
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if recv, ok := fn.Recv.List[0].Type.(*ast.StarExpr); !ok || exprName(recv.X) != "API" {
				continue
			}
			method := func(e ast.Expr) string {
				name, ok := strings.CutPrefix(exprName(e), "http.Method")
				if !ok {
					t.Errorf("%s: %s: unknown HTTP method expression", fset.Position(e.Pos()), fn.Name.Name)
				}
				return strings.ToUpper(name)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					switch exprName(n.Type) {
					case "route.Endpoint":
						tables[fn.Name.Name] = append(tables[fn.Name.Name], routeDecl{method: method(field(n, "Method"))})
					case "route.Subroute":
						action := ""
						if lit, ok := field(n, "Action").(*ast.BasicLit); ok {
							action, _ = strconv.Unquote(lit.Value)
						}
						tables[fn.Name.Name] = append(tables[fn.Name.Name], routeDecl{method: method(field(n, "Method")), action: action, sub: true})
					}
				case *ast.BinaryExpr:
					if n.Op == token.NEQ && exprName(n.X) == "r.Method" {
						tables[fn.Name.Name] = append(tables[fn.Name.Name], routeDecl{method: method(n.Y)})
					}
				}
				return true
			})
		}
	}
	return tables
}

func routepathConstants(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "uikit", "routepath", "routepath.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
		}
	}
	return out
}

func resolvePattern(e ast.Expr, consts map[string]string) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "routepath" {
			s, ok := consts[e.Sel.Name]
			return s, ok
		}
	}
	return "", false
}

func field(lit *ast.CompositeLit, key string) ast.Expr {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok && exprName(kv.Key) == key {
			return kv.Value
		}
	}
	return nil
}

// exprName renders an identifier or a selector chain such as http.MethodGet.
func exprName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprName(e.X) + "." + e.Sel.Name
	}
	return ""
}
