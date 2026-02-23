package restv1

type LoginRequest struct {
	Subject  string `json:"subject"`
	Password string `json:"password"`
}

type LoginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	SessionID    string `json:"session_id"`
}

type LogoutRequest struct {
	SessionID string `json:"session_id"`
}
