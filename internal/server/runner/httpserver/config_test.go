package httpserver

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"address", Config{Addr: ":8080"}, nil},
		{"host and port", Config{Addr: "127.0.0.1:0"}, nil},
		{"ipv6", Config{Addr: "[::1]:8080"}, nil},
		{"zero values keep the defaults", Config{Addr: ":8080", ReadTimeout: 0, MaxHeaderBytes: 0}, nil},
		{"empty", Config{}, ErrEmptyAddr},
		{"no port", Config{Addr: "localhost"}, ErrInvalidAddr},
		{"port only", Config{Addr: "8080"}, ErrInvalidAddr},
		{"named port", Config{Addr: ":http"}, ErrInvalidAddr},
		{"port out of range", Config{Addr: ":70000"}, ErrInvalidAddr},
		{"negative read timeout", Config{Addr: ":8080", ReadTimeout: -time.Second}, ErrNegativeValue},
		{"negative header bytes", Config{Addr: ":8080", MaxHeaderBytes: -1}, ErrNegativeValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
