// Package httpcookie carries auth tokens over HTTP cookies.
// Owns the three auth-cookie names and their SameSite/Secure policy.
package httpcookie

import "net/http"

// Cookie names used by the auth flow.
const (
	NameAccessToken  = "access_token"
	NameRefreshToken = "refresh_token"
	NameSessionID    = "session_id"
)

// SetAuth writes the three auth cookies to the response.
func SetAuth(w http.ResponseWriter, r *http.Request, accessToken, refreshToken, sessionID string) {
	secure := r.TLS != nil

	http.SetCookie(w, &http.Cookie{
		Name:     NameAccessToken,
		Value:    accessToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     NameRefreshToken,
		Value:    refreshToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     NameSessionID,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// DeleteAuth expires all three auth cookies.
func DeleteAuth(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil

	http.SetCookie(w, &http.Cookie{
		Name:     NameAccessToken,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     NameRefreshToken,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     NameSessionID,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// GetAccessToken returns the access_token cookie.
func GetAccessToken(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(NameAccessToken)
}

// GetRefreshToken returns the refresh_token cookie.
func GetRefreshToken(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(NameRefreshToken)
}

// GetSessionID returns the session_id cookie.
func GetSessionID(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(NameSessionID)
}
