package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/event"
)

func TestLoadEventsCoalesce(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		loaded          time.Duration // events.coalesce as loaded
		effective       time.Duration // with the defaults, as the hub gets it
		errorPath       string
	}{
		{name: "default", loaded: time.Second, effective: time.Second},
		{name: "yaml", yaml: "events:\n  coalesce: 250ms\n", loaded: 250 * time.Millisecond, effective: 250 * time.Millisecond},
		{name: "env", env: "2s", loaded: 2 * time.Second, effective: 2 * time.Second},
		{name: "env wins over yaml", yaml: "events:\n  coalesce: 250ms\n", env: "3s", loaded: 3 * time.Second, effective: 3 * time.Second},
		{name: "zero keeps the default", env: "0s", effective: time.Second},
		{name: "negative", env: "-1s", errorPath: "config.events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ""
			if tc.yaml != "" {
				path = filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CONFIG_PATH", path)
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			if tc.env != "" {
				t.Setenv("SOLTI_EVENTS_COALESCE", tc.env)
			}
			cfg, err := load("")
			if tc.errorPath != "" {
				if !errors.Is(err, event.ErrNegativeCoalesce) || !strings.Contains(err.Error(), tc.errorPath) {
					t.Fatalf("want a %s error wrapping ErrNegativeCoalesce, got %v", tc.errorPath, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Events.Coalesce != tc.loaded {
				t.Fatalf("events.coalesce = %v, want %v", cfg.Events.Coalesce, tc.loaded)
			}
			if got := cfg.Events.WithDefaults().Coalesce; got != tc.effective {
				t.Fatalf("events.coalesce with the defaults = %v, want %v", got, tc.effective)
			}
		})
	}
}
