package handlers

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/soltiHQ/control-plane/ui/pages"
)

type Auth struct {
	logger zerolog.Logger
}

func NewAuth(logger zerolog.Logger) *Auth {
	return &Auth{logger: logger.With().Str("handler", "auth").Logger()}
}

func (h *Auth) SignIn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	_ = pages.SignInPage().Render(r.Context(), w)
}
