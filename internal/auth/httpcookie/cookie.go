// Package httpcookie carries auth tokens over HTTP cookies.
// Owns the three auth-cookie names and their SameSite policy; the caller
// decides Secure (auth.cookie_secure).
package httpcookie

import "net/http"

// Cookie names used by the auth flow.
const (
	NameAccessToken  = "access_token"
	NameRefreshToken = "refresh_token"
	NameSessionID    = "session_id"
)

// SetAuth writes the three auth cookies to the response, with the Secure
// attribute when secure is set.
func SetAuth(w http.ResponseWriter, secure bool, accessToken, refreshToken, sessionID string) {
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

// DeleteAuth expires all three auth cookies, with the Secure attribute when
// secure is set.
func DeleteAuth(w http.ResponseWriter, secure bool) {
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
