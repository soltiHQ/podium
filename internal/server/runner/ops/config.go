package ops

import (
	"github.com/soltiHQ/control-plane/internal/server/runner/httpserver"
)

const defaultName = "ops"

// Config controls the ops listener. An empty Addr disables it.
type Config struct {
	Name string `yaml:"name"`
	Addr string `yaml:"addr"`
}

// Enabled reports whether the listener runs.
func (c Config) Enabled() bool { return c.Addr != "" }

// HTTP is the configuration of the runner that serves the listener: the name
// defaults to "ops", which keeps it apart from the other listeners.
func (c Config) HTTP() httpserver.Config {
	if c.Name == "" {
		c.Name = defaultName
	}
	return httpserver.Config{Name: c.Name, Addr: c.Addr}
}

// Validate reports a malformed address; an empty one disables the listener.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	return c.HTTP().Validate()
}
