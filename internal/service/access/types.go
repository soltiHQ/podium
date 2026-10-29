package access

// LoginRequest is an authentication request.
type LoginRequest struct {
	Subject  string
	Password string
	// ClientIP is where the attempt came from: failures count per account and per client IP.
	ClientIP string
}

// LoginResult carries issued tokens and session ID after a successful login.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	SessionID    string
}

// LogoutRequest revokes a session.
type LogoutRequest struct {
	SessionID string
}
