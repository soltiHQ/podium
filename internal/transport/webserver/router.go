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
		handlerErrors = handlers.NewErrors(s.logger)
		mux           = http.NewServeMux()
	)

	// static
	mux.Handle("/static/", handlerStatic)

	// api
	mux.HandleFunc("/api/demo/status", handlerDemo.Status)
	mux.HandleFunc("/api/demo/time", handlerDemo.Time)

	// pages
	mux.HandleFunc("/about", handlerPage.About)
	mux.HandleFunc("/503", handlerErrors.ServiceUnavailable)

	// home + fallback 404
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			handlerErrors.NotFound(w, r)
			return
		}
		handlerPage.Home(w, r)
	})

	return mux
}
