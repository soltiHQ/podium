// Package middleware provides HTTP middleware for the control-plane server.
//
//   - RequestID           ensures every request carries a unique ID for log correlation.
//   - Auth                verifies access tokens and stores identity in context; handles silent refresh.
//   - OptionalAuth        Auth that lets callers without valid credentials through, without an identity.
//   - RequirePermission   guards routes by checking identity permissions.
//   - Recheck             repeats Auth and RequirePermission for a long-lived stream.
//   - Negotiate           chooses the Responder (JSON / HTML) per request.
//   - CrossOrigin         refuses cross-origin browser mutations (http.CrossOriginProtection).
//   - Logger              structured request/response logging with zerolog.
//   - Recovery            catches panics and renders a 503 response.
//   - CORS                configurable cross-origin resource sharing.
package middleware
