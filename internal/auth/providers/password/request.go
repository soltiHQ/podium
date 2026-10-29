package password

import "github.com/soltiHQ/control-plane/domain/enum"

// Request carries password credentials for authentication.
type Request struct {
	Subject  string
	Password string
}

func (Request) AuthKind() enum.Auth { return enum.Password }
