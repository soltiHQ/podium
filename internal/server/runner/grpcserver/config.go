package grpcserver

import (
	"fmt"
	"net"
	"strconv"
)

const (
	defaultName    = "grpc"
	defaultNetwork = "tcp"
)

// Config controls the gRPC server runner behavior.
type Config struct {
	Name    string `yaml:"name"`
	Addr    string `yaml:"addr"`
	Network string `yaml:"network"`
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = defaultName
	}
	if c.Network == "" {
		c.Network = defaultNetwork
	}
	return c
}

// Validate reports an empty address, and for a TCP network (the default) one
// that is not host:port with a numeric port.
func (c Config) Validate() error {
	if c.Addr == "" {
		return ErrEmptyAddr
	}
	if network := c.withDefaults().Network; network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil
	}
	if _, port, err := net.SplitHostPort(c.Addr); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidAddr, c.Addr)
	} else if _, err = strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidAddr, c.Addr)
	}
	return nil
}
