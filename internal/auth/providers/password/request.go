package password

import "github.com/soltiHQ/control-plane/domain/kind"

// Request carries password credentials for authentication.
type Request struct {
	Subject  string
	Password string
}

func (Request) AuthKind() kind.Auth { return kind.Password }
