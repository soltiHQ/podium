package handlers

import (
	"net/http"
)

// RenderMode defines how an HTTP response should be rendered.
type RenderMode int

const (
	RenderPage RenderMode = iota
	RenderBlock
)

// Fault provides a centralized, format-aware entry point for HTTP error responses.
//
// It encapsulates rendering logic for different failure scenarios (e.g., 404, 401, 503)
// and delegates to the appropriate responder (HTML, JSON, etc.) based on request context.
type Fault struct{}

// NewFault creates a new fault handler.
func NewFault() *Fault {
	return &Fault{}
}

// NotFound renders a 404 response.
//func (f *Fault) NotFound(w http.ResponseWriter, r *http.Request, mode RenderMode) {
//	httpctx.Responder(r.Context()).Respond(w, r, http.StatusNotFound, &responder.View{
//		Data: responder.ErrorBody{
//			Code:    http.StatusNotFound,
//			Message: "not found",
//		},
//		Component: func(m RenderMode) templ.Component {
//			if m == RenderPage {
//				return pages.ErrorPage(
//					http.StatusNotFound,
//					"Page not found",
//					"The page you are looking for doesn't exist or has been moved.",
//				)
//			}
//			return nil
//		}(mode),
//	})
//}

// Unauthorized renders a 401 response.
//func (f *Fault) Unauthorized(w http.ResponseWriter, r *http.Request, mode RenderMode) {
//	httpctx.Responder(r.Context()).Respond(w, r, http.StatusUnauthorized, &responder.View{
//		Data: responder.ErrorBody{
//			Code:    http.StatusUnauthorized,
//			Message: "unauthorized",
//		},
//		Component: func(m RenderMode) templ.Component {
//			if m == RenderPage {
//				return pages.ErrorPage(
//					http.StatusUnauthorized,
//					"Unauthorized",
//					"You are not authorized to access this page.",
//				)
//			}
//			return nil
//		}(mode),
//	})
//}

// Unavailable renders a 503 response.
//func (f *Fault) Unavailable(w http.ResponseWriter, r *http.Request, mode RenderMode) {
//	httpctx.Responder(r.Context()).Respond(w, r, http.StatusUnauthorized, &responder.View{
//		Data: responder.ErrorBody{
//			Code:    http.StatusServiceUnavailable,
//			Message: "service unavailable",
//		},
//		Component: func(m RenderMode) templ.Component {
//			if m == RenderPage {
//				return pages.ErrorPage(
//					http.StatusServiceUnavailable,
//					"Service unavailable",
//					"The server is temporarily unable to handle the request.",
//				)
//			}
//			return nil
//		}(mode),
//	})
//}

// AuthRateLimit renders a 429 response.
//func (f *Fault) AuthRateLimit(w http.ResponseWriter, r *http.Request, mode RenderMode) {
//	httpctx.Responder(r.Context()).Respond(w, r, http.StatusTooManyRequests, &responder.View{
//		Data: responder.ErrorBody{
//			Code:    http.StatusTooManyRequests,
//			Message: "too many auth requests",
//		},
//		Component: func(m RenderMode) templ.Component {
//			if m == RenderPage {
//				return pages.ErrorPage(
//					http.StatusTooManyRequests,
//					"Too many auth attempts",
//					"Account temporarily locked. Please try again later.",
//				)
//			}
//			return nil
//		}(mode),
//	})
//}

// Wrap a main handler and render 404 for unmatched routes.
func (f *Fault) Wrap(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			//f.NotFound(w, r, RenderPage)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
