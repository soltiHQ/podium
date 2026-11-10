// Package buildinfo reports which podium is running: its version, commit, build
// time and toolchain, read from the build information that the Go toolchain
// embeds in the binary ([debug.ReadBuildInfo]). Nothing is injected at link time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// commitLen is how many hex digits of the commit the version line shows.
const commitLen = 12

// unknown stands for a part of the line the binary does not carry.
const unknown = "unknown"

// Info is what the binary knows about its build.
type Info struct {
	// Version is the version of the main module: the tag a build from a tag
	// carries, "dev" for a build the toolchain gave no version ("(devel)").
	Version string `json:"version"`
	// Commit is the VCS revision, empty when the build had none.
	Commit string `json:"commit,omitempty"`
	// Modified reports a build from a tree with uncommitted changes.
	Modified bool `json:"modified"`
	// BuildTime is the time of the commit, zero when the build had none.
	BuildTime time.Time `json:"build_time,omitzero"`
	// GoVersion is the toolchain, such as go1.27.3.
	GoVersion string `json:"go_version"`
	// Platform is GOOS/GOARCH of the build.
	Platform string `json:"platform"`
}

// Read returns the Info of the running binary.
func Read() Info {
	return from(debug.ReadBuildInfo())
}

// from is [Read] over the given build information.
func from(bi *debug.BuildInfo, ok bool) Info {
	info := Info{
		Version:   "dev",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if !ok || bi == nil {
		return info
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = v
	}
	if bi.GoVersion != "" {
		info.GoVersion = bi.GoVersion
	}
	var goos, goarch string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				info.BuildTime = t
			}
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		}
	}
	if goos != "" && goarch != "" {
		info.Platform = goos + "/" + goarch
	}
	return info
}

// String is the version line:
//
//	podium <version> (commit <12 hex>[, modified], built <RFC 3339>, <go version> <os>/<arch>)
//
// A binary built without VCS information shows "unknown" for the commit and the
// build time.
func (i Info) String() string {
	commit, built := unknown, unknown
	if i.Commit != "" {
		commit = i.Commit[:min(len(i.Commit), commitLen)]
	}
	if !i.BuildTime.IsZero() {
		built = i.BuildTime.UTC().Format(time.RFC3339)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "podium %s (commit %s", i.Version, commit)
	if i.Modified {
		b.WriteString(", modified")
	}
	fmt.Fprintf(&b, ", built %s, %s %s)", built, i.GoVersion, i.Platform)
	return b.String()
}
