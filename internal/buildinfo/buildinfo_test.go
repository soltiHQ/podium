package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

func settings(kv ...string) []debug.BuildSetting {
	var out []debug.BuildSetting
	for i := 0; i < len(kv); i += 2 {
		out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return out
}

// The line is one of podium's commands' output: version, the first 12 hex digits
// of the commit, "modified" for a dirty tree, the build time in RFC 3339, the Go
// version and the platform.
func TestVersionLine(t *testing.T) {
	const full = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{
			name: "a tagged build",
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.3", Main: debug.Module{Version: "v1.4.0"},
				Settings: settings("vcs.revision", full, "vcs.modified", "false", "vcs.time", "2026-11-05T10:20:30Z", "GOOS", "linux", "GOARCH", "amd64"),
			},
			want: "podium v1.4.0 (commit 0123456789ab, built 2026-11-05T10:20:30Z, go1.27.3 linux/amd64)",
		},
		{
			name: "a build from a modified tree reports dev",
			bi: &debug.BuildInfo{
				GoVersion: "go1.27.3", Main: debug.Module{Version: "(devel)"},
				Settings: settings("vcs.revision", full, "vcs.modified", "true", "vcs.time", "2026-11-05T10:20:30Z", "GOOS", "darwin", "GOARCH", "arm64"),
			},
			want: "podium dev (commit 0123456789ab, modified, built 2026-11-05T10:20:30Z, go1.27.3 darwin/arm64)",
		},
		{
			name: "no VCS information",
			bi:   &debug.BuildInfo{GoVersion: "go1.27.3", Main: debug.Module{Version: "(devel)"}, Settings: settings("GOOS", "linux", "GOARCH", "arm64")},
			want: "podium dev (commit unknown, built unknown, go1.27.3 linux/arm64)",
		},
		{
			name: "an empty module version",
			bi:   &debug.BuildInfo{GoVersion: "go1.27.3", Settings: settings("vcs.revision", "abc", "GOOS", "linux", "GOARCH", "arm64")},
			want: "podium dev (commit abc, built unknown, go1.27.3 linux/arm64)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := from(tc.bi, true).String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A binary without build information still has a line, from the runtime.
func TestFromWithoutBuildInformation(t *testing.T) {
	info := from(nil, false)
	if info.Version != "dev" || info.Commit != "" || info.GoVersion != runtime.Version() || info.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("from(nil, false) = %+v", info)
	}
	if want := "podium dev (commit unknown, built unknown, " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"; info.String() != want {
		t.Fatalf("String() = %q, want %q", info.String(), want)
	}
}

func TestFromParsesTheBuildTime(t *testing.T) {
	info := from(&debug.BuildInfo{Settings: settings("vcs.time", "2026-11-05T10:20:30Z")}, true)
	if want := time.Date(2026, 11, 5, 10, 20, 30, 0, time.UTC); !info.BuildTime.Equal(want) {
		t.Fatalf("BuildTime = %v, want %v", info.BuildTime, want)
	}
	if bad := from(&debug.BuildInfo{Settings: settings("vcs.time", "yesterday")}, true); !bad.BuildTime.IsZero() {
		t.Fatalf("BuildTime = %v for an unparsable time, want zero", bad.BuildTime)
	}
}

// Read works in a test binary, which carries build information but no VCS stamp.
func TestReadHasAGoVersionAndPlatform(t *testing.T) {
	info := Read()
	if info.Version == "" || info.GoVersion == "" || info.Platform == "" {
		t.Fatalf("Read() = %+v, want a version, a Go version and a platform", info)
	}
}
