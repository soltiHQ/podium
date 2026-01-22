package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui"
)

type Static struct {
	logger  zerolog.Logger
	handler http.Handler
}

func NewStatic(logger zerolog.Logger) *Static {
	fs := http.FS(ui.Static)

	return &Static{
		logger:  logger.With().Str("handler", "static").Logger(),
		handler: http.StripPrefix("/static/", http.FileServer(fs)),
	}
}

func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}
