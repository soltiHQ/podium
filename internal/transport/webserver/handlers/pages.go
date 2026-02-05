package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui/pages"
)

// Pages serves templ-compiled page components.
type Pages struct {
	logger zerolog.Logger
}

// NewPages creates a new Pages handler.
// NOTE: renderer is no longer needed - templ handles rendering.
func NewPages(logger zerolog.Logger) *Pages {
	return &Pages{
		logger: logger.With().Str("handler", "pages").Logger(),
	}
}

// Home renders the home page.
func (p *Pages) Home(w http.ResponseWriter, r *http.Request) {
	if err := pages.Home().Render(r.Context(), w); err != nil {
		p.logger.Error().Err(err).Msg("failed to render home page")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
}

// About renders the about page.
func (p *Pages) About(w http.ResponseWriter, r *http.Request) {
	if err := pages.About().Render(r.Context(), w); err != nil {
		p.logger.Error().Err(err).Msg("failed to render about page")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
}
