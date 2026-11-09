package event

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/config/settings"
	"gopkg.in/yaml.v3"
)

func TestConfigCoalesce(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		wantRaw         time.Duration // as loaded
		want            time.Duration // with the defaults
		valid           bool
	}{
		{name: "default", wantRaw: DefaultCoalesce, want: time.Second, valid: true},
		{name: "yaml", yaml: "coalesce: 250ms\n", wantRaw: 250 * time.Millisecond, want: 250 * time.Millisecond, valid: true},
		{name: "env", env: "2s", wantRaw: 2 * time.Second, want: 2 * time.Second, valid: true},
		{name: "env wins over yaml", yaml: "coalesce: 250ms\n", env: "3s", wantRaw: 3 * time.Second, want: 3 * time.Second, valid: true},
		{name: "zero keeps the default", env: "0s", wantRaw: 0, want: time.Second, valid: true},
		{name: "negative", yaml: "coalesce: -1s\n", wantRaw: -time.Second, want: -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("SOLTI_EVENTS_COALESCE", tc.env)
			}
			if err := settings.ApplyEnv("SOLTI_EVENTS", &cfg, os.LookupEnv); err != nil {
				t.Fatal(err)
			}
			if cfg.Coalesce != tc.wantRaw {
				t.Fatalf("coalesce = %v, want %v", cfg.Coalesce, tc.wantRaw)
			}
			if got := cfg.WithDefaults().Coalesce; got != tc.want {
				t.Fatalf("coalesce with defaults = %v, want %v", got, tc.want)
			}
			err := cfg.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate() = %v, want valid", err)
			}
			if !tc.valid && !errors.Is(err, ErrNegativeCoalesce) {
				t.Fatalf("Validate() = %v, want ErrNegativeCoalesce", err)
			}
		})
	}
}
