package kind

// Auth defines the authentication mechanism type.
type Auth string

const (
	Password Auth = "password"
	APIKey   Auth = "api_key"
)
