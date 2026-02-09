package handlers

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/response"
	"github.com/soltiHQ/control-plane/ui/pages"
)

// Errors provides format-aware error responses.
type Errors struct{}

// NewErrors creates a new error handler.
func NewErrors() *Errors {
	return &Errors{}
}

// NotFound renders a 404 response.
func (e *Errors) NotFound(w http.ResponseWriter, r *http.Request) {
	response.FromContext(r.Context()).Respond(w, r, http.StatusNotFound, &response.View{
		Data: response.ErrorBody{
			Code:    http.StatusNotFound,
			Message: "not found",
		},
		Component: pages.ErrorPage(
			http.StatusNotFound,
			"Page not found",
			"The page you are looking for doesn't exist or has been moved.",
		),
	})
}

// Unauthorized renders a 401 response.
func (e *Errors) Unauthorized(w http.ResponseWriter, r *http.Request) {
	response.FromContext(r.Context()).Respond(w, r, http.StatusUnauthorized, &response.View{
		Data: response.ErrorBody{
			Code:    http.StatusUnauthorized,
			Message: "unauthorized",
		},
		Component: pages.ErrorPage(
			http.StatusUnauthorized,
			"Unauthorized",
			"You are not authorized to access this page.",
		),
	})
}

// ServiceUnavailable renders a 503 response.
func (e *Errors) ServiceUnavailable(w http.ResponseWriter, r *http.Request) {
	response.FromContext(r.Context()).Respond(w, r, http.StatusServiceUnavailable, &response.View{
		Data: response.ErrorBody{
			Code:    http.StatusServiceUnavailable,
			Message: "service unavailable",
		},
		Component: pages.ErrorPage(
			http.StatusServiceUnavailable,
			"Service unavailable",
			"The server is temporarily unable to handle the request.",
		),
	})
}

func (e *Errors) ManyAuthAttempts(w http.ResponseWriter, r *http.Request) {
	response.FromContext(r.Context()).Respond(w, r, http.StatusTooManyRequests, &response.View{
		Data: response.ErrorBody{
			Code:    http.StatusTooManyRequests,
			Message: "too many auth requests",
		},
		Component: pages.ErrorPage(
			http.StatusTooManyRequests,
			"Too many auth attempts",
			"Account temporarily locked. Please try again later.",
		),
	})
}

// Wrap a handler and renders 404 for unmatched routes.
func (e *Errors) Wrap(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			e.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
