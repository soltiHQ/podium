package settings_test

import (
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/config/settings"
)

type inner struct {
	Wait time.Duration `yaml:"wait_for"`
	Name string        `yaml:"name"`
}

type sample struct {
	Flag   bool     `yaml:"flag"`
	Count  int      `yaml:"count"`
	Big    int64    `yaml:"big"`
	Small  int8     `yaml:"small"`
	List   []string `yaml:"list"`
	Inner  inner    `yaml:"inner"`
	Plain  string   // no tag: the lower-cased field name
	Secret string   `yaml:"-" env:"APP_SECRET"`
	Hidden string   `yaml:"-"` // neither a key nor a variable
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { v, ok := env[name]; return v, ok }
}

func TestApplyEnvParsesEveryType(t *testing.T) {
	var got sample
	err := settings.ApplyEnv("APP", &got, lookup(map[string]string{
		"APP_FLAG": "true", "APP_COUNT": "-3", "APP_BIG": "9000000000", "APP_SMALL": "127",
		"APP_LIST": " a , b,c ", "APP_INNER_WAIT_FOR": "1m30s", "APP_INNER_NAME": " kept as is ",
		"APP_PLAIN": "p", "APP_SECRET": "s3cret", "HIDDEN": "x", "APP_HIDDEN": "x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Flag || got.Count != -3 || got.Big != 9_000_000_000 || got.Small != 127 ||
		strings.Join(got.List, "|") != "a|b|c" || got.Inner.Wait != 90*time.Second ||
		got.Inner.Name != " kept as is " || got.Plain != "p" || got.Secret != "s3cret" || got.Hidden != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestApplyEnvErrorsNameTheVariableNotTheValue(t *testing.T) {
	for _, tc := range []struct{ name, value, says string }{
		{"APP_FLAG", "yes please", "not a valid boolean"},
		{"APP_COUNT", "12x", "not a valid integer"},
		{"APP_SMALL", "128", "not a valid integer"}, // out of int8's range
		{"APP_INNER_WAIT_FOR", "soon", "not a valid duration"},
		{"APP_COUNT", "", "is empty"},
	} {
		var got sample
		err := settings.ApplyEnv("APP", &got, lookup(map[string]string{tc.name: tc.value}))
		if err == nil || !strings.Contains(err.Error(), tc.name+": "+tc.says) {
			t.Errorf("%s=%q: error = %v, want %q", tc.name, tc.value, err, tc.says)
		} else if tc.value != "" && strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s: error %q shows the value", tc.name, err)
		}
	}
	// An empty string and an empty list are values, not mistakes.
	got := sample{Plain: "x", List: []string{"y"}}
	if err := settings.ApplyEnv("APP", &got, lookup(map[string]string{"APP_PLAIN": "", "APP_LIST": ""})); err != nil || got.Plain != "" || got.List != nil {
		t.Fatalf("empty string and list: %+v, %v", got, err)
	}
}

func TestWalkNamesAndPaths(t *testing.T) {
	var got []string
	for _, s := range settings.Walk("APP", &sample{}) {
		got = append(got, s.Env+"="+s.Key())
	}
	want := "APP_FLAG=flag APP_COUNT=count APP_BIG=big APP_SMALL=small APP_LIST=list " +
		"APP_INNER_WAIT_FOR=inner.wait_for APP_INNER_NAME=inner.name APP_PLAIN=plain APP_SECRET=secret"
	if strings.Join(got, " ") != want {
		t.Fatalf("walk = %s\nwant   %s", strings.Join(got, " "), want)
	}
}

func TestWalkPanicsOnATypeNoVariableCanSet(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "Opts") {
			t.Fatalf("recover() = %v, want a panic that names the field", r)
		}
	}()
	settings.Walk("APP", &struct{ Opts map[string]string }{})
}

// Two settings whose paths join to one variable name, or an env-only setting
// that takes the name another setting derives, would both be set by that one
// variable: Walk panics, naming the variable and both settings.
func TestWalkPanicsOnTwoSettingsOfOneVariable(t *testing.T) {
	type httpSection struct {
		DiscoveryAddr string `yaml:"discovery_addr"`
	}
	type discoverySection struct {
		Addr string `yaml:"addr"`
	}
	for _, tc := range []struct {
		name string
		dst  any
		want []string
	}{
		{"derived names", &struct {
			HTTP      httpSection      `yaml:"http"`
			Discovery discoverySection `yaml:"http_discovery"`
		}{}, []string{"APP_HTTP_DISCOVERY_ADDR", "http.discovery_addr", "http_discovery.addr"}},
		// An env-only setting's key is derived from its variable.
		{"an env-only name", &struct {
			Discovery discoverySection `yaml:"http_discovery"`
			Secret    string           `yaml:"-" env:"APP_HTTP_DISCOVERY_ADDR"`
		}{}, []string{"APP_HTTP_DISCOVERY_ADDR", "http_discovery.addr", "http_discovery_addr"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				for _, want := range tc.want {
					if !strings.Contains(msg, want) {
						t.Fatalf("recover() = %q, want a panic that names %s", msg, strings.Join(tc.want, ", "))
					}
				}
			}()
			settings.Walk("APP", tc.dst)
		})
	}
}

func TestClosest(t *testing.T) {
	names := []string{"SOLTI_HTTP_ADDR", "SOLTI_HTTP_NAME", "SOLTI_GRPC_ADDR"}
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"SOLTI_HTTP_ADRR", "SOLTI_HTTP_ADDR", true},
		{"SOLTI_HTPP_ADDR", "SOLTI_HTTP_ADDR", true},
		{"SOLTI_HTTP_ADDRESS", "", false}, // three edits away
		{"SOLTI_CONTROL_PLANE", "", false},
	} {
		got, ok := settings.Closest(tc.in, names)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Closest(%s) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
