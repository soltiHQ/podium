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
	mux.Handle("/static/", handlerStatic)
	mux.HandleFunc("GET /", handlerPage.Home)
	return mux
}
