package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui/pages"
)

type Errors struct {
	logger zerolog.Logger
}

func NewErrors(logger zerolog.Logger) *Errors {
	return &Errors{logger: logger.With().Str("handler", "errors").Logger()}
}

func (h *Errors) NotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)

	_ = pages.ErrorPage(
		404,
		"Page not found",
		"The page you are looking for doesn't exist or has been moved.",
	).Render(r.Context(), w)
}

func (h *Errors) ServiceUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)

	_ = pages.ErrorPage(
		503,
		"Service Unavailable",
		"We're temporarily down for maintenance. Please check back in a few moments.",
	).Render(r.Context(), w)
}
