package webserver

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/webserver/handlers"
)

func (s *WebServer) router() http.Handler {
	var (
		handlerPage   = handlers.NewPages(s.logger)
		handlerDemo   = handlers.NewDemo(s.logger)
		handlerStatic = handlers.NewStatic(s.logger)
		mux           = http.NewServeMux()
	)

	// Static assets
	mux.HandleFunc("GET /static/", handlerStatic.ServeHTTP)
	mux.HandleFunc("HEAD /static/", handlerStatic.ServeHTTP)

	// Pages (templ-rendered)
	mux.HandleFunc("GET /", handlerPage.Home)
	mux.HandleFunc("GET /about", handlerPage.About)

	// HTMX API endpoints
	mux.HandleFunc("GET /api/demo/status", handlerDemo.Status)
	mux.HandleFunc("GET /api/demo/time", handlerDemo.Time)

	return mux
}
