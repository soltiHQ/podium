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

	discoverv1 "github.com/soltiHQ/control-plane/domain/gen/v1"
	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/svc"
	"github.com/soltiHQ/control-plane/internal/backend"
	"github.com/soltiHQ/control-plane/internal/handlers"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/grpcserver"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// Storage
	store := inmemory.New()

	// Bootstrap admin user
	if err := bootstrap(context.Background(), store); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}
	logger.Info().Msg("bootstrap: admin/admin created")

	// Auth stack
	jwtSecret := "dev-secret-change-me-in-production"
	authSVC := svc.NewAuth(
		store,
		jwtSecret,
		1*time.Minute,
		7*24*time.Hour,
		1*time.Minute,
		2,
	)

	// Responders
	jsonResp := responder.NewJSON()
	htmlResp := responder.NewHTML()

	// Backend
	loginUC := backend.NewLogin(authSVC)

	// Discovery backend (важно: не store напрямую, а UC)
	// Если у тебя конструктор называется иначе — меняешь только эту строку.
	discoveryUC := backend.NewDiscovery(store)
	agentsUC := backend.NewAgents(store)

	// Handlers
	uiHandler := handlers.NewUI(logger, authSVC, loginUC, agentsUC)
	apiHandler := handlers.NewAPI(logger, authSVC, loginUC)
	staticHandler := handlers.NewStatic(logger)

	httpDiscovery := handlers.NewHTTPDiscovery(logger, discoveryUC)
	grpcDiscovery := handlers.NewGRPCDiscovery(logger, discoveryUC)

	// ---------------------------------------------------------------
	// UI/API HTTP :8080
	// ---------------------------------------------------------------
	mux := http.NewServeMux()

	staticHandler.Routes(mux)

	// Public
	mux.HandleFunc("/login", uiHandler.Login)
	mux.HandleFunc("/api/v1/login", apiHandler.Login)

	// Protected
	authMw := middleware.Auth(authSVC.Verifier, authSVC.Session)
	mux.Handle("/", authMw(http.HandlerFunc(uiHandler.Main)))
	mux.Handle("/agents", authMw(http.HandlerFunc(uiHandler.Agents)))

	// Middleware chain (outer -> inner)
	var handler http.Handler = mux
	handler = middleware.Negotiate(jsonResp, htmlResp)(handler)
	handler = middleware.Recovery(logger)(handler)
	handler = middleware.Logger(logger)(handler)
	handler = middleware.RequestID()(handler)
	handler = middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
	})(handler)

	httpRunner, err := httpserver.New(
		httpserver.Config{Name: "http", Addr: ":8080"},
		logger,
		handler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	// ---------------------------------------------------------------
	// HTTP Discovery :8082
	// ---------------------------------------------------------------
	discMux := http.NewServeMux()
	discMux.HandleFunc("/api/v1/discovery/sync", httpDiscovery.Sync)

	// Тут negotiate НЕ нужен: путь /api/* и так вернёт JSON,
	// но middleware.Response у тебя опирается на Responder из ctx.
	// Поэтому negotiate оставляем, иначе response.* будет падать в TryResponder()->NewJSON()
	// (если тебе это норм — можешь убрать, но тогда HTMLResponder там не нужен).
	var discHandler http.Handler = discMux
	discHandler = middleware.Negotiate(jsonResp, htmlResp)(discHandler)
	discHandler = middleware.Recovery(logger)(discHandler)
	discHandler = middleware.Logger(logger)(discHandler)
	discHandler = middleware.RequestID()(discHandler)

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
	grpcSrv := grpc.NewServer()
	discoverv1.RegisterDiscoverServiceServer(grpcSrv, grpcDiscovery)

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

	logger.Info().Msg("starting servers: http=:8080, http-discovery=:8082, grpc=:50051")

	if err := srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}

	logger.Info().Msg("server stopped")
}

// bootstrap seeds an admin user with all permissions.
func bootstrap(ctx context.Context, store *inmemory.Store) error {
	role, err := model.NewRole("role-admin", "admin")
	if err != nil {
		return err
	}
	for _, p := range kind.All {
		if err := role.PermissionAdd(p); err != nil {
			return err
		}
	}
	if err := store.UpsertRole(ctx, role); err != nil {
		return err
	}

	user, err := model.NewUser("user-admin", "admin")
	if err != nil {
		return err
	}
	user.NameAdd("Admin")
	user.EmailAdd("admin@local")
	if err := user.RoleAdd("role-admin"); err != nil {
		return err
	}
	if err := store.UpsertUser(ctx, user); err != nil {
		return err
	}

	cred, err := credentials.NewPasswordCredential("cred-admin", "user-admin", "admin", 0)
	if err != nil {
		return err
	}
	if err := store.UpsertCredential(ctx, cred); err != nil {
		return err
	}

	return nil
}
