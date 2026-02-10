package handlers

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui"
)

type Static struct {
	logger zerolog.Logger
	fs     http.FileSystem
}

func NewStatic(logger zerolog.Logger) *Static {
	sub, err := fs.Sub(ui.Static, "static")
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load static files")
	}
	return &Static{
		logger: logger,
		fs:     http.FS(sub),
	}
}

func (s *Static) Routes(mux *http.ServeMux) {
	mux.Handle("/static/", http.StripPrefix("/static/", http.HandlerFunc(s.serve)))

	mux.HandleFunc("/static", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/", http.StatusMovedPermanently)
	})
}

func (s *Static) serve(w http.ResponseWriter, r *http.Request) {
	//w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	//w.Header().Set("X-Content-Type-Options", "nosniff")

	if strings.HasSuffix(r.URL.Path, "/") {
		http.NotFound(w, r)
		return
	}
	http.FileServer(s.fs).ServeHTTP(w, r)
}
