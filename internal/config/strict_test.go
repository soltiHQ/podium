package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/internal/config/settings"
)

// writeConfig writes body to a config file and points CONFIG_PATH at it with a
// valid JWT secret in the environment; it returns the path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	return path
}

// An unknown key fails loading at any depth, naming the file, its line, the key's
// full path and the keys known at that level.
func TestUnknownKeyFailsWithPathAndLine(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "top level",
			body: "# settings\nlisten: 1\n",
			want: `config: %s:2: unknown key "listen" (known: agent_auth, auth, cluster, cors, events, grpc, history, http, http_discovery, lifecycle, server, streams, sync, tls, triggers, trusted_proxies)`,
		},
		{
			name: "nested",
			body: "http:\n  addr: :8080\n  lisen: 1\n",
			want: `config: %s:3: unknown key "http.lisen" (known: addr, idle_timeout, max_header_bytes, name, read_header_timeout, read_timeout, write_timeout)`,
		},
		{
			name: "three levels down",
			body: "cluster:\n  raft:\n    tls:\n      cert_fil: x\n",
			want: `config: %s:4: unknown key "cluster.raft.tls.cert_fil" (known: ca_file, cert_file, key_file)`,
		},
		{
			name: "the key yaml.v3 would derive for an env-only field",
			body: "agent_auth:\n  enrollsecret: x\n",
			want: `config: %s:2: unknown key "agent_auth.enrollsecret" (known: require)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			_, err := load("")
			if err == nil || err.Error() != strings.Replace(tc.want, "%s", path, 1) {
				t.Fatalf("error = %v, want %s", err, strings.Replace(tc.want, "%s", path, 1))
			}
		})
	}
}

// A setting that only the environment may carry fails loading when the file holds
// it, and the error names the variable, never the value.
func TestEnvOnlyKeyInFileFails(t *testing.T) {
	for _, tc := range []struct{ name, body, key, env string }{
		{"enrollment secret", "agent_auth:\n  require: true\n  enroll_secret: file-secret-value\n", "agent_auth.enroll_secret", "SOLTI_AGENT_AUTH_ENROLL_SECRET"},
		{"bootstrap password", "# first\n\nbootstrap_admin_password: file-secret-value\n", "bootstrap_admin_password", "SOLTI_BOOTSTRAP_ADMIN_PASSWORD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			_, err := load("")
			if err == nil || !strings.Contains(err.Error(), path+":") ||
				!strings.Contains(err.Error(), tc.key+" is read from "+tc.env+" only; remove it from the file") {
				t.Fatalf("error = %v, want %s read from %s only", err, tc.key, tc.env)
			}
			if strings.Contains(err.Error(), "file-secret-value") {
				t.Fatalf("error %q shows the value", err)
			}
		})
	}
}

// A file without a setting is a mistake, not a configuration.
func TestEmptyFileFails(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"comments only", "# nothing here\n# yet\n"},
		{"document marker", "---\n"},
		{"empty mapping", "{}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			_, err := load("")
			if want := "config: " + path + " holds no settings"; err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

// leaf is a setting of the configuration in the test's own words: the variable
// name derived here from the yaml tags, independent of the loader.
type leaf struct {
	env  string
	path string
	val  reflect.Value
}

func leaves(t *testing.T, v reflect.Value, path []string, out []leaf) []leaf {
	t.Helper()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "-" {
			if env := f.Tag.Get("env"); env != "" {
				out = append(out, leaf{env, strings.Join(path, ".") + "." + f.Name, v.Field(i)})
			}
			continue
		}
		if tag == "" {
			tag = strings.ToLower(f.Name)
		}
		sub := append(slices.Clone(path), tag)
		if f.Type.Kind() == reflect.Struct {
			out = leaves(t, v.Field(i), sub, out)
			continue
		}
		out = append(out, leaf{"SOLTI_" + strings.ToUpper(strings.Join(sub, "_")), strings.Join(sub, "."), v.Field(i)})
	}
	return out
}

// Every setting of the default configuration is read from SOLTI_ and its YAML
// path in upper case, parts joined by "_", and from no other name.
func TestEnvNamesFollowYAMLPaths(t *testing.T) {
	cfg := Default()
	all := leaves(t, reflect.ValueOf(&cfg).Elem(), nil, nil)
	if len(all) < 80 {
		t.Fatalf("found only %d settings, the walk is broken", len(all))
	}
	for _, name := range []string{
		"SOLTI_HTTP_READ_HEADER_TIMEOUT", "SOLTI_HTTP_DISCOVERY_ADDR", "SOLTI_CLUSTER_RAFT_TLS_CA_FILE",
		"SOLTI_TRUSTED_PROXIES", "SOLTI_AGENT_AUTH_ENROLL_SECRET", "SOLTI_BOOTSTRAP_ADMIN_PASSWORD",
	} {
		if !slices.ContainsFunc(all, func(l leaf) bool { return l.env == name }) {
			t.Errorf("no setting is read from %s", name)
		}
	}
	want := make(map[string]any, len(all))
	for _, l := range all {
		var raw string
		switch typ := l.val.Type(); {
		case typ == reflect.TypeOf(time.Duration(0)):
			raw, want[l.env] = "90s", 90*time.Second
		case typ.Kind() == reflect.Bool:
			raw, want[l.env] = strconv.FormatBool(!l.val.Bool()), !l.val.Bool()
		case typ.Kind() == reflect.Int || typ.Kind() == reflect.Int64:
			raw, want[l.env] = "1234", 1234
			if typ.Kind() == reflect.Int64 {
				want[l.env] = int64(1234)
			}
		case typ.Kind() == reflect.Slice:
			raw, want[l.env] = " a ,b,  c d", []string{"a", "b", "c d"}
		default:
			raw, want[l.env] = "value-"+l.env, "value-"+l.env
		}
		t.Setenv(l.env, raw)
	}
	got := Default()
	if err := applyEnv(&got); err != nil {
		t.Fatal(err)
	}
	for _, l := range leaves(t, reflect.ValueOf(&got).Elem(), nil, nil) {
		got := l.val.Interface()
		if l.val.Kind() == reflect.String {
			got = l.val.String() // cluster.backend has a named string type
		}
		if !reflect.DeepEqual(got, want[l.env]) {
			t.Errorf("%s (%s) = %#v, want %#v", l.env, l.path, got, want[l.env])
		}
	}
}

// A bare name, without SOLTI_ and the path in front, changes nothing.
func TestBareEnvNamesAreIgnored(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	before, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"HOSTNAME": "podium.example.com", "PORT": "9999", "DATA_DIR": "/elsewhere", "REQUIRE": "false",
		"ENROLL_SECRET": strings.Repeat("e", 32), "ISSUER": "someone", "CERT_FILE": "/c", "KEY_FILE": "/k",
		"CA_FILE": "/ca", "JOIN": "true", "DRIVER": "dns", "PEERS": "n1=10.0.0.1:7000", "BIND_ADDR": "1.2.3.4:5",
		"BOOTSTRAP_ADMIN_PASSWORD": "from-a-bare-name",
	} {
		t.Setenv(name, value)
	}
	after, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("bare names changed the configuration:\nbefore %+v\nafter  %+v", before, after)
	}
}

// An unknown SOLTI_ variable under a podium setting's prefix fails loading; any
// other one, such as the SDK's, is logged at WARN once, with the closest setting
// when one is within edit distance 2. Neither shows the value.
func TestUnknownSoltiEnv(t *testing.T) {
	const value = "do-not-show-me"
	for _, tc := range []struct {
		name    string
		fails   bool
		suggest string
	}{
		{name: "SOLTI_HTTP_ADRR", fails: true, suggest: "SOLTI_HTTP_ADDR"},
		{name: "SOLTI_AGENT_AUTH_ENROL_SECRET", fails: true, suggest: "SOLTI_AGENT_AUTH_ENROLL_SECRET"},
		{name: "SOLTI_CLUSTER_SOMETHING_ELSE_ENTIRELY", fails: true},
		{name: "SOLTI_HTPP_ADDR", suggest: "SOLTI_HTTP_ADDR"},
		{name: "SOLTI_CONTROL_PLANE"},
		{name: "SOLTI_AGENT_ADVERTISE_URL"},
		{name: "SOLTI_CONTAINERD_ENDPOINT"},
		{name: "SOLTI_METRICS_ADDR"},
		{name: "SOLTI_TASK_ENV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv(tc.name, value)
			var logs bytes.Buffer
			_, err := Load("", zerolog.New(&logs))
			if tc.fails {
				want := "unknown environment variable " + tc.name
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
				if tc.suggest != "" && !strings.Contains(err.Error(), tc.suggest) {
					t.Fatalf("error %q lacks the suggestion %s", err, tc.suggest)
				}
				if tc.suggest == "" && strings.Contains(err.Error(), "did you mean") {
					t.Fatalf("error %q suggests a name that is not near", err)
				}
				if strings.Contains(err.Error(), value) {
					t.Fatalf("error %q shows the value", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("an unknown variable outside the settings' prefixes failed loading: %v", err)
			}
			out := logs.String()
			if strings.Count(out, tc.name) != 1 || !strings.Contains(out, `"level":"warn"`) || strings.Contains(out, value) {
				t.Fatalf("log = %q, want one warning that names %s and not its value", out, tc.name)
			}
			if tc.suggest != "" && !strings.Contains(out, tc.suggest) {
				t.Fatalf("log = %q, want the suggestion %s", out, tc.suggest)
			}
			if tc.suggest == "" && strings.Contains(out, "suggestion") {
				t.Fatalf("log = %q suggests a name that is not near", out)
			}
		})
	}
}

// A value that does not parse fails loading with the variable's name, not the
// value.
func TestEnvValueErrorsNameTheVariable(t *testing.T) {
	for _, tc := range []struct{ name, value, says string }{
		{"SOLTI_HTTP_READ_TIMEOUT", "secret-ish", "not a valid duration"},
		{"SOLTI_AGENT_AUTH_REQUIRE", "secret-ish", "not a valid boolean"},
		{"SOLTI_SYNC_MAX_RETRIES", "secret-ish", "not a valid integer"},
		{"SOLTI_SYNC_MAX_CONCURRENCY", "", "is empty"},
		{"SOLTI_HISTORY_ARCHIVE_MAX_BYTES", "99999999999999999999", "not a valid integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv(tc.name, tc.value)
			_, err := load("")
			if err == nil || !strings.Contains(err.Error(), tc.name+": "+tc.says) {
				t.Fatalf("error = %v, want %s: %s", err, tc.name, tc.says)
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error %q shows the value", err)
			}
		})
	}
}

// Every section of the configuration is validated when it loads, so that a bad
// value fails before any listener or store opens: one row per section.
func TestEverySectionValidatedAtLoad(t *testing.T) {
	rows := []struct{ section, env, value string }{
		{"history", "SOLTI_HISTORY_ARCHIVE_RETENTION", "-1s"},
		{"http", "SOLTI_HTTP_ADDR", ""},
		{"http_discovery", "SOLTI_HTTP_DISCOVERY_ADDR", "8082"},
		{"grpc", "SOLTI_GRPC_ADDR", ":70000"},
		{"sync", "SOLTI_SYNC_TICK_INTERVAL", "-1s"},
		{"lifecycle", "SOLTI_LIFECYCLE_MAX_CONCURRENCY", "-1"},
		{"triggers", "SOLTI_TRIGGERS_DASHBOARD_REFRESH", "often"},
		{"server", "SOLTI_SERVER_SHUTDOWN_TIMEOUT", "-1s"},
		{"auth", "SOLTI_AUTH_COOKIE_SECURE", "never"},
		{"cors", "SOLTI_CORS_ALLOW_ORIGINS", "https://app.example.com/"},
		{"cluster", "SOLTI_CLUSTER_BACKEND", "paxos"},
		{"events", "SOLTI_EVENTS_COALESCE", "-1s"},
		{"streams", "SOLTI_STREAMS_SUBSCRIBER_BUFFER", "-1"},
		{"tls", "SOLTI_TLS_SERVER_CERT_FILE", "/only/a/cert.pem"},
		{"agent_auth", "SOLTI_AGENT_AUTH_ENROLL_SECRET", "short"},
		{"trusted_proxies", "SOLTI_TRUSTED_PROXIES", "proxy.example.com"},
	}
	cfg := Default()
	for _, section := range append(settings.Sections(&cfg), "trusted_proxies") {
		if !slices.ContainsFunc(rows, func(r struct{ section, env, value string }) bool { return r.section == section }) {
			t.Errorf("no row for the %s section", section)
		}
	}
	for _, tc := range rows {
		t.Run(tc.section, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv(tc.env, tc.value)
			_, err := load("")
			// A section reports as config.<section>, or config.<section>.<part> when it has parts.
			if err == nil || !strings.Contains(err.Error(), "config."+tc.section+":") && !strings.Contains(err.Error(), "config."+tc.section+".") {
				t.Fatalf("error = %v, want one that names config.%s", err, tc.section)
			}
			if errors.Is(err, errUnknownEnv) {
				t.Fatalf("error = %v, want a validation error", err)
			}
		})
	}
}
