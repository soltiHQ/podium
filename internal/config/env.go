package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/config/settings"
)

// errUnknownEnv is the cause of a SOLTI_ variable that falls under a setting's
// prefix but names no setting.
var errUnknownEnv = errors.New("unknown environment variable")

// applyEnv sets every setting that the environment carries, by the name that
// follows its YAML path.
func applyEnv(cfg *Config) error {
	return settings.ApplyEnv(envPrefix, cfg, os.LookupEnv)
}

// checkEnv looks at the SOLTI_ variables of environ that name no setting. One
// that falls under the prefix of a section (SOLTI_HTTP_ for http) is a mistyped
// setting and fails loading, with the closest setting name when one is within
// edit distance 2. The solti
// SDK and its examples share the SOLTI_ prefix (SOLTI_CONTROL_PLANE,
// SOLTI_AGENT_ADVERTISE_URL, SOLTI_CONTAINERD_*, ...) and may sit in the same
// shell, so any other name is only warned about, once. Values are never shown.
func checkEnv(cfg *Config, environ []string, log zerolog.Logger) error {
	var (
		known    = make(map[string]struct{})
		names    []string
		prefixes []string
		errs     []error
	)
	for _, s := range settings.Walk(envPrefix, cfg) {
		known[s.Env] = struct{}{}
		names = append(names, s.Env)
	}
	for _, section := range settings.Sections(cfg) {
		prefixes = append(prefixes, envPrefix+"_"+strings.ToUpper(section)+"_")
	}
	var unknown []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := known[name]; ok || !strings.HasPrefix(name, envPrefix+"_") {
			continue
		}
		unknown = append(unknown, name)
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		near, found := settings.Closest(name, names)
		if hasPrefix(name, prefixes) {
			err := fmt.Errorf("%w %s", errUnknownEnv, name)
			if found {
				err = fmt.Errorf("%w (did you mean %s?)", err, near)
			}
			errs = append(errs, err)
			continue
		}
		event := log.Warn().Str("variable", name)
		if found {
			event = event.Str("suggestion", near)
		}
		event.Msg("ignoring an environment variable that is not a podium setting")
	}
	return errors.Join(errs...)
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
