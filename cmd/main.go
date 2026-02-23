package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	genv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/wire"
	"github.com/soltiHQ/control-plane/internal/handler"
	"github.com/soltiHQ/control-plane/internal/proxy"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/service/access"
	"github.com/soltiHQ/control-plane/internal/service/agent"
	"github.com/soltiHQ/control-plane/internal/service/credential"
	"github.com/soltiHQ/control-plane/internal/service/session"
	"github.com/soltiHQ/control-plane/internal/service/user"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/http/route"
)

func main() {
	var (
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
		store  = inmemory.New()
	)

	// Bootstrap admin user
	if err := bootstrap(context.Background(), store); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}
	///

	var (
		jwtSecret = "dev-secret-change-me-in-production"
		authModel = wire.NewAuth(
			store,
			jwtSecret,
			1*time.Minute,
			7*24*time.Hour,
			1*time.Minute,
			2,
		)
	)

	var (
		authSVC       = access.New(authModel, store)
		userSVC       = user.New(store, logger)
		sessionSVC    = session.New(store, logger)
		credentialSVC = credential.New(store, logger)
		agentSVC      = agent.New(store, logger)
	)

	proxyPool := proxy.NewPool()
	defer proxyPool.Close()

	var (
		jsonResp = responder.NewJSON()
		htmlResp = responder.NewHTML()
	)
	var (
		uiHandler     = handler.NewUI(logger, authSVC)
		apiHandler    = handler.NewAPI(logger, userSVC, authSVC, sessionSVC, credentialSVC, agentSVC, proxyPool)
		staticHandler = handler.NewStatic(logger)
	)
	authMW := middleware.Auth(authModel.Verifier, authModel.Session)
	permMW := route.PermMW(func(p kind.Permission) route.BaseMW {
		return middleware.RequirePermission(p)
	})

	mux := http.NewServeMux()
	staticHandler.Routes(mux)
	uiHandler.Routes(mux,
		authMW,
		permMW,
	)
	apiHandler.Routes(mux,
		authMW,
		permMW,
	)

	var mainHandler http.Handler = mux
	mainHandler = middleware.Negotiate(jsonResp, htmlResp)(mainHandler)
	mainHandler = middleware.Recovery(logger)(mainHandler)

	httpRunner, err := httpserver.New(
		httpserver.Config{Name: "http", Addr: ":8080"},
		logger,
		mainHandler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	// ---------------------------------------------------------------
	// HTTP Discovery :8082
	// ---------------------------------------------------------------
	httpDiscovery := handler.NewHTTPDiscovery(logger, agentSVC)

	discMux := http.NewServeMux()
	discMux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	var discHandler http.Handler = discMux
	discHandler = middleware.Recovery(logger)(discHandler)

	httpDiscoveryRunner, err := httpserver.New(
		httpserver.Config{Name: "http-discovery", Addr: ":8082"},
		logger,
		discHandler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http discovery server")
	}

	// ---------------------------------------------------------------
	// gRPC Discovery :50051
	// ---------------------------------------------------------------
	grpcDiscovery := handler.NewGRPCDiscovery(logger, agentSVC)

	grpcSrv := grpc.NewServer()
	genv1.RegisterDiscoverServiceServer(grpcSrv, grpcDiscovery)

	grpcRunner, err := grpcserver.New(
		grpcserver.Config{Name: "grpc-discovery", Addr: ":50051"},
		logger,
		grpcSrv,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create grpc server")
	}

	// ---------------------------------------------------------------
	// Server (3 runners)
	// ---------------------------------------------------------------
	srv, err := server.New(server.Config{}, logger, httpRunner, httpDiscoveryRunner, grpcRunner)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create server")
	}

	// Run
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}
	logger.Info().Msg("server stopped")
}

func bootstrap(ctx context.Context, store *inmemory.Store) error {
	// ---------------------------------------------------
	// ROLES
	// ---------------------------------------------------

	// admin (all permissions)
	adminRole, err := model.NewRole("role-admin", "admin")
	if err != nil {
		return err
	}
	for _, p := range kind.All {
		if err := adminRole.PermissionAdd(p); err != nil {
			return err
		}
	}
	if err := store.UpsertRole(ctx, adminRole); err != nil {
		return err
	}

	// agents-read
	agentsReadRole, err := model.NewRole("role-agents-read", "agents-read")
	if err != nil {
		return err
	}
	_ = agentsReadRole.PermissionAdd(kind.AgentsGet)
	if err := store.UpsertRole(ctx, agentsReadRole); err != nil {
		return err
	}

	// agents-editor
	agentsEditRole, err := model.NewRole("role-agents-edit", "agents-edit")
	if err != nil {
		return err
	}
	_ = agentsEditRole.PermissionAdd(kind.AgentsGet)
	_ = agentsEditRole.PermissionAdd(kind.AgentsEdit)
	if err := store.UpsertRole(ctx, agentsEditRole); err != nil {
		return err
	}

	// users-read
	usersReadRole, err := model.NewRole("role-users-read", "users-read")
	if err != nil {
		return err
	}
	_ = usersReadRole.PermissionAdd(kind.UsersGet)
	if err := store.UpsertRole(ctx, usersReadRole); err != nil {
		return err
	}

	// users-manager
	usersManagerRole, err := model.NewRole("role-users-manager", "users-manager")
	if err != nil {
		return err
	}
	_ = usersManagerRole.PermissionAdd(kind.UsersGet)
	_ = usersManagerRole.PermissionAdd(kind.UsersAdd)
	_ = usersManagerRole.PermissionAdd(kind.UsersEdit)
	if err := store.UpsertRole(ctx, usersManagerRole); err != nil {
		return err
	}

	// readonly (agents + users get)
	readOnlyRole, err := model.NewRole("role-readonly", "readonly")
	if err != nil {
		return err
	}
	_ = readOnlyRole.PermissionAdd(kind.AgentsGet)
	_ = readOnlyRole.PermissionAdd(kind.UsersGet)
	if err := store.UpsertRole(ctx, readOnlyRole); err != nil {
		return err
	}

	// ---------------------------------------------------
	// USERS
	// ---------------------------------------------------

	type seedUser struct {
		ID       string
		Subject  string
		Name     string
		Email    string
		RoleID   string
		Password string
	}

	users := []seedUser{
		{
			ID:       "user-admin",
			Subject:  "admin",
			Name:     "Admin",
			Email:    "admin@local",
			RoleID:   "role-admin",
			Password: "admin",
		},
		{
			ID:       "user-agents-read",
			Subject:  "agent_reader",
			Name:     "Agent Reader",
			Email:    "agent.reader@local",
			RoleID:   "role-agents-read",
			Password: "password",
		},
		{
			ID:       "user-agents-edit",
			Subject:  "agent_editor",
			Name:     "Agent Editor",
			Email:    "agent.editor@local",
			RoleID:   "role-agents-edit",
			Password: "password",
		},
		{
			ID:       "user-users-read",
			Subject:  "user_reader",
			Name:     "User Reader",
			Email:    "user.reader@local",
			RoleID:   "role-users-read",
			Password: "password",
		},
		{
			ID:       "user-users-manager",
			Subject:  "user_manager",
			Name:     "User Manager",
			Email:    "user.manager@local",
			RoleID:   "role-users-manager",
			Password: "password",
		},
		{
			ID:       "user-readonly",
			Subject:  "readonly",
			Name:     "Read Only",
			Email:    "readonly@local",
			RoleID:   "role-readonly",
			Password: "password",
		},
	}

	for _, u := range users {
		user, err := model.NewUser(u.ID, u.Subject)
		if err != nil {
			return err
		}
		user.NameAdd(u.Name)
		user.EmailAdd(u.Email)
		if err := user.RoleAdd(u.RoleID); err != nil {
			return err
		}
		if err := store.UpsertUser(ctx, user); err != nil {
			return err
		}

		// ---------------------------------------------------
		// CREDENTIAL + VERIFIER (password)
		// ---------------------------------------------------

		credID := "cred-" + u.ID
		verID := "ver-" + u.ID

		cred, err := model.NewCredential(credID, u.ID, kind.Password)
		if err != nil {
			return err
		}
		if err := store.UpsertCredential(ctx, cred); err != nil {
			return err
		}

		ver, err := credentials.NewPasswordVerifier(
			verID,
			credID,
			u.Password,
			0, // cost (0 => default)
		)
		if err != nil {
			return err
		}
		if err := store.UpsertVerifier(ctx, ver); err != nil {
			return err
		}
	}

	return nil
}
