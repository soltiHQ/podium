package grpcserver

import (
	"errors"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"address", Config{Addr: ":50051"}, nil},
		{"empty", Config{}, ErrEmptyAddr},
		{"no port", Config{Addr: "localhost"}, ErrInvalidAddr},
		{"port out of range", Config{Addr: ":70000"}, ErrInvalidAddr},
		{"tcp6", Config{Network: "tcp6", Addr: "50051"}, ErrInvalidAddr},
		{"a unix socket is a path", Config{Network: "unix", Addr: "/run/podium.sock"}, nil},
		{"a unix socket still needs one", Config{Network: "unix"}, ErrEmptyAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
