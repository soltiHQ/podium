package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/internal/auth/ratelimit"

	"github.com/soltiHQ/control-plane/domain/kind"
	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/auth/auth/session"
	"github.com/soltiHQ/control-plane/internal/auth/credentials"
	"github.com/soltiHQ/control-plane/internal/auth/providers"
	passwordprovider "github.com/soltiHQ/control-plane/internal/auth/providers/password"
	"github.com/soltiHQ/control-plane/internal/auth/rbac"
	"github.com/soltiHQ/control-plane/internal/auth/token"
	"github.com/soltiHQ/control-plane/internal/auth/token/jwt"
	"github.com/soltiHQ/control-plane/internal/handlers"
	"github.com/soltiHQ/control-plane/internal/server"
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
	"github.com/soltiHQ/control-plane/internal/storage/inmemory"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
	"github.com/soltiHQ/control-plane/internal/transport/http/response"
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	// ---------------------------------------------------------------
	// Storage
	// ---------------------------------------------------------------
	store := inmemory.New()

	// ---------------------------------------------------------------
	// Bootstrap admin user
	// ---------------------------------------------------------------
	if err := bootstrap(context.Background(), store); err != nil {
		logger.Fatal().Err(err).Msg("failed to bootstrap")
	}
	logger.Info().Msg("bootstrap: admin/admin created")

	// ---------------------------------------------------------------
	// Auth stack
	// ---------------------------------------------------------------
	jwtSecret := []byte("dev-secret-change-me-in-production")
	clk := token.RealClock()

	issuer := jwt.NewHSIssuer(jwtSecret, clk)
	verifier := jwt.NewHSVerifier("control-plane", "control-plane", jwtSecret, clk)
	resolver := rbac.NewResolver(store)

	sessionSvc := session.New(
		store,
		issuer,
		clk,
		session.Config{
			AccessTTL:     15 * time.Minute,
			RefreshTTL:    7 * 24 * time.Hour,
			Issuer:        "control-plane",
			Audience:      "control-plane",
			RotateRefresh: true,
		},
		resolver,
		map[kind.Auth]providers.Provider{
			kind.Password: passwordprovider.New(store),
		},
	)

	loginLimiter := ratelimit.New(ratelimit.Config{
		MaxAttempts: 2,
		BlockWindow: 10 * time.Minute,
	})

	// ---------------------------------------------------------------
	// Responders & Handlers
	// ---------------------------------------------------------------
	jsonResp := response.NewJSON()
	htmlResp := response.NewHTML(response.HTMLConfig{})

	demo := handlers.NewDemo(jsonResp)
	errHandler := handlers.NewErrors()
	authHandler := handlers.NewAuth(sessionSvc, jsonResp, loginLimiter, clk)
	uiHandler := handlers.NewUI(logger, sessionSvc, store, htmlResp, loginLimiter, clk, errHandler)
	staticHandler := handlers.NewStatic(logger)

	// ---------------------------------------------------------------
	// Router
	// ---------------------------------------------------------------
	mux := http.NewServeMux()

	// Public — no auth.
	authHandler.Routes(mux) // POST /v1/login
	uiHandler.Routes(mux)   // GET /login, POST /login
	staticHandler.Routes(mux)

	// Protected — auth required.
	authMw := middleware.Auth(verifier)
	mux.Handle("GET /api/hello", authMw(http.HandlerFunc(demo.Hello)))

	var handler http.Handler = errHandler.Wrap(mux)
	handler = response.Negotiate(jsonResp, htmlResp)(handler)
	handler = middleware.Recovery(logger)(handler)
	handler = middleware.Logger(logger)(handler)
	handler = middleware.RequestID()(handler)
	handler = middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
	})(handler)

	// ---------------------------------------------------------------
	// Global middleware chain (outer → inner)
	// CORS → RequestID → Logger → Recovery → Router
	// ---------------------------------------------------------------
	//var handler http.Handler = mux
	//handler = middleware.Recovery(logger)(handler)
	//handler = middleware.Logger(logger)(handler)
	//handler = middleware.RequestID()(handler)
	//handler = middleware.CORS(middleware.CORSConfig{
	//	AllowOrigins: []string{"*"},
	//})(handler)

	// ---------------------------------------------------------------
	// Server
	// ---------------------------------------------------------------
	httpRunner, err := httpserver.New(
		httpserver.Config{Name: "http", Addr: ":8080"},
		logger,
		handler,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create http server")
	}

	srv, err := server.New(server.Config{}, logger, httpRunner)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create server")
	}

	// ---------------------------------------------------------------
	// Run
	// ---------------------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info().Msg("starting server on :8080")

	if err := srv.Run(ctx); err != nil {
		logger.Error().Err(err).Msg("server exited")
		os.Exit(1)
	}

	logger.Info().Msg("server stopped")
}

// bootstrap seeds an admin user with all permissions.
func bootstrap(ctx context.Context, store *inmemory.Store) error {
	// Role with all permissions.
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

	// User.
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

	// Password credential.
	cred, err := credentials.NewPasswordCredential("cred-admin", "user-admin", "admin", 0)
	if err != nil {
		return err
	}
	if err := store.UpsertCredential(ctx, cred); err != nil {
		return err
	}

	return nil
}
