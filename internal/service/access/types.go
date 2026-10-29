package access

// LoginRequest is an authentication request.
type LoginRequest struct {
	Subject  string
	Password string
	// ClientIP is where the attempt came from: failures count per account and per client IP.
	ClientIP string
	// DeviceCookie is the raw device cookie the client sent, if any. One that
	// is valid for the subject makes failures count per device instead.
	DeviceCookie string
}

// LoginResult carries issued tokens and session ID after a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	SessionID    string
	// DeviceCookie is a new device cookie value for the client to keep.
	DeviceCookie string
}

// LogoutRequest revokes a session.
type LogoutRequest struct {
	SessionID string
}
