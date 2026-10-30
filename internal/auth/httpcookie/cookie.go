// Package httpcookie carries auth tokens over HTTP cookies.
// Owns the names and attributes of the three auth cookies and of the device
// cookie; the caller decides Secure (auth.cookie_secure).
package httpcookie

import (
	"net/http"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth/devicecookie"
)

// Cookie names used by the auth flow.
const (
	NameAccessToken  = "access_token"
	NameRefreshToken = "refresh_token"
	NameSessionID    = "session_id"
	// NameDeviceToken marks a browser that logged in before (devicecookie).
	NameDeviceToken = "device_token"
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

// SetDevice writes the device cookie, with the Secure attribute when secure
// is set. The browser sends it only to path, the login route, and keeps it as
// long as it stays valid (devicecookie.MaxAge); a new one replaces it.
func SetDevice(w http.ResponseWriter, secure bool, path, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     NameDeviceToken,
		Value:    value,
		Path:     path,
		MaxAge:   int(devicecookie.MaxAge / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
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

// GetDevice returns the device_token cookie.
func GetDevice(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(NameDeviceToken)
}
