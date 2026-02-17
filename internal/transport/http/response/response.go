package response

import (
	"net/http"

	"github.com/soltiHQ/control-plane/internal/transport/http/responder"
	"github.com/soltiHQ/control-plane/internal/transport/httpctx"
	"github.com/soltiHQ/control-plane/internal/transportctx"

	pageSystem "github.com/soltiHQ/control-plane/ui/templates/page/system"

	"github.com/a-h/templ"
)

// OK renders a 200 response.
func OK(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode, v *responder.View) {
	httpctx.Responder(r.Context()).Respond(w, r, http.StatusOK, v)
}

// NoContent renders a 204 response.
func NoContent(w http.ResponseWriter, r *http.Request) {
	httpctx.Responder(r.Context()).Respond(w, r, http.StatusNoContent, nil)
}

// json error body.
type errorBody struct {
	Code      int    `json:"code"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// NotFound renders a 404 response.
func NotFound(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusNotFound, &responder.View{
		Data: errorBody{
			Code:      http.StatusNotFound,
			Message:   "not found",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusNotFound,
					"Page not found",
					"The page you are looking for doesn't exist or has been moved.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// BadRequest renders a 400 response.
func BadRequest(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusBadRequest, &responder.View{
		Data: errorBody{
			Code:      http.StatusBadRequest,
			Message:   "invalid request",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusBadRequest,
					"Invalid request",
					"The request you sent is invalid.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// NotAllowed renders a 405 response.
func NotAllowed(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusMethodNotAllowed, &responder.View{
		Data: errorBody{
			Code:      http.StatusMethodNotAllowed,
			Message:   "not allowed",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusMethodNotAllowed,
					"Method not allowed",
					"The method you used is not allowed on this resource.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// Forbidden renders a 403 response.
func Forbidden(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusForbidden, &responder.View{
		Data: errorBody{
			Code:      http.StatusForbidden,
			Message:   "forbidden",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusForbidden,
					"Forbidden",
					"You are not allowed to access this page.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// Unauthorized renders a 401 response.
func Unauthorized(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusUnauthorized, &responder.View{
		Data: errorBody{
			Code:      http.StatusUnauthorized,
			Message:   "unauthorized",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusUnauthorized,
					"Unauthorized",
					"You are not authorized to access this page.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// Unavailable renders a 503 response.
func Unavailable(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusServiceUnavailable, &responder.View{
		Data: errorBody{
			Code:      http.StatusServiceUnavailable,
			Message:   "service unavailable",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusServiceUnavailable,
					"Service unavailable",
					"The server is temporarily unable to handle the request.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}

// AuthRateLimit renders a 429 response.
func AuthRateLimit(w http.ResponseWriter, r *http.Request, mode httpctx.RenderMode) {
	reqId := transportctx.TryRequestID(r.Context())

	httpctx.Responder(r.Context()).Respond(w, r, http.StatusTooManyRequests, &responder.View{
		Data: errorBody{
			Code:      http.StatusTooManyRequests,
			Message:   "too many auth requests",
			RequestID: reqId,
		},
		Component: func(m httpctx.RenderMode) templ.Component {
			if m == httpctx.RenderPage {
				return pageSystem.ErrorPage(
					http.StatusTooManyRequests,
					"Too many auth attempts",
					"Account temporarily locked. Please try again later.",
					reqId,
				)
			}
			return nil
		}(mode),
	})
}
