package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
)

// Pages represents a set of handlers for serving static pages.
type Pages struct {
	logger   zerolog.Logger
	renderer Renderer
}

// NewPages creates a new Pages instance.
func NewPages(logger zerolog.Logger, renderer Renderer) *Pages {
	return &Pages{
		logger:   logger.With().Str("handler", "pages").Logger(),
		renderer: renderer,
	}
}

// Home serves the home page.
func (p *Pages) Home(w http.ResponseWriter, _ *http.Request) {
	type ViewData struct {
		Title string
	}

	if err := p.renderer.Render(w, "home.html", ViewData{
		Title: "Control Plane",
	}); err != nil {
		p.logger.Error().Err(err).Msg("failed to render home page")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
}
