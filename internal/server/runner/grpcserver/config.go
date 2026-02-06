package grpcserver

// Config controls the grpc server runner behavior.
type Config struct {
	Name string
	Addr string
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "grpc"
	}
	return c
}
