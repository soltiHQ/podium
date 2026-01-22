package webserver

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/webserver/handlers"
)

func (s *WebServer) router() http.Handler {
	var (
		handlerPage   = handlers.NewPages(s.logger, s.render)
		handlerStatic = handlers.NewStatic(s.logger)
		mux           = http.NewServeMux()
	)
	mux.HandleFunc("GET /static/", handlerStatic.ServeHTTP)
	mux.HandleFunc("HEAD /static/", handlerStatic.ServeHTTP)
	mux.HandleFunc("GET /", handlerPage.Home)
	return mux
}
