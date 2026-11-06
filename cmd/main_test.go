package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExitCodeReflectsRunAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		run, cleanup error
		want         int
	}{{"clean", nil, nil, 0}, {"signal", context.Canceled, nil, 0}, {"cleanup timeout", nil, context.DeadlineExceeded, 1}, {"runner failure", errors.New("failure"), nil, 1}, {"signal with incomplete drain", errors.Join(context.Canceled, context.DeadlineExceeded), nil, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.run, tc.cleanup); got != tc.want {
				t.Fatalf("exit %d want %d", got, tc.want)
			}
		})
	}
}

// enrollSecret is a 32-byte secret; the tokens below were computed outside
// podium (openssl dgst -sha256 -hmac), as the lowercase hex of HMAC-SHA256 of
// the agent ID under it.
const enrollSecret = "0123456789abcdef0123456789abcdef"

// runAgentToken runs "podium agent-token args..." with secret in the
// environment; unset removes the variable instead.
func runAgentToken(t *testing.T, secret string, unset bool, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", secret)
	if unset {
		if err := os.Unsetenv("SOLTI_AGENT_AUTH_ENROLL_SECRET"); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	code = agentToken(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// "podium agent-token <agent-id>" prints exactly the token the agent enrolls
// with while the secret is set, then a newline, and nothing else.
func TestAgentTokenCommandPrintsDerivedToken(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"agent-1", "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058"},
		{"agent-2", "14202fea7ea2cc313572d5d8e8c424489c954421b0f6160ce9e07d27340644fc"},
		{"edge.node_7-b", "4168feeb732b1e4d186ff475faf224bca00c52923f6d0e96846a965c51c8cd51"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, enrollSecret, false, tc.id)
			if code != 0 || stdout != tc.want+"\n" || stderr != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0, stdout %q and no stderr", code, stdout, stderr, tc.want+"\n")
			}
			if strings.Contains(stdout+stderr, enrollSecret) {
				t.Fatal("the command printed the secret")
			}
		})
	}
}

// A missing or short secret, an invalid agent ID and a wrong argument count
// each end with a nonzero exit and one short line on stderr: no token on
// stdout, and never the secret.
func TestAgentTokenCommandRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		unset        bool
		args         []string
		want         int
	}{
		{name: "no agent ID", secret: enrollSecret, want: 2},
		{name: "two agent IDs", secret: enrollSecret, args: []string{"agent-1", "agent-2"}, want: 2},
		{name: "empty agent ID", secret: enrollSecret, args: []string{""}, want: 1},
		{name: "agent ID with a space", secret: enrollSecret, args: []string{"agent 1"}, want: 1},
		{name: "agent ID of dots", secret: enrollSecret, args: []string{".."}, want: 1},
		{name: "agent ID over 128 bytes", secret: enrollSecret, args: []string{strings.Repeat("a", 129)}, want: 1},
		{name: "secret unset", unset: true, args: []string{"agent-1"}, want: 1},
		{name: "secret empty", secret: "", args: []string{"agent-1"}, want: 1},
		{name: "secret of 31 bytes", secret: strings.Repeat("s", 31), args: []string{"agent-1"}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, tc.secret, tc.unset, tc.args...)
			if code != tc.want {
				t.Fatalf("exit %d, want %d", code, tc.want)
			}
			if stdout != "" {
				t.Fatalf("stdout %q, want nothing", stdout)
			}
			if !strings.HasSuffix(stderr, "\n") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("stderr %q, want one line", stderr)
			}
			if tc.secret != "" && strings.Contains(stderr, tc.secret) {
				t.Fatal("the command printed the secret")
			}
		})
	}
}

// A bare -h or --help prints the usage to stdout and exits 0; with any other
// argument it is no help request.
func TestAgentTokenCommandHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, "", true, flag)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q; want exit 0 and no stderr", code, stderr)
			}
			if !strings.HasPrefix(stdout, "usage: podium agent-token [--generation N] [--] <agent-id>\n") {
				t.Fatalf("stdout %q, want the usage", stdout)
			}
		})
	}
	if code, stdout, _ := runAgentToken(t, enrollSecret, false, "-h", "agent-1"); code != 2 || stdout != "" {
		t.Fatalf("-h with an agent ID: exit %d, stdout %q; want the usage error", code, stdout)
	}
}

// "--" ends the options: the argument after it is the agent ID, even one that
// looks like a flag. "--" alone names no agent.
func TestAgentTokenCommandAcceptsDoubleDash(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"agent-1", []string{"--", "agent-1"}, "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058"},
		{"an agent named -h", []string{"--", "-h"}, "59a897d1df79f408eb013456a314522d9f4edee24d902870707bc9c0d9a7ea4e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, enrollSecret, false, tc.args...)
			if code != 0 || stdout != tc.want+"\n" || stderr != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and stdout %q", code, stdout, stderr, tc.want+"\n")
			}
		})
	}
	for _, args := range [][]string{{"--"}, {"--", "agent-1", "agent-2"}} {
		if code, stdout, stderr := runAgentToken(t, enrollSecret, false, args...); code != 2 || stdout != "" || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("args %q: exit %d, stdout %q, stderr %q; want the usage error", args, code, stdout, stderr)
		}
	}
}

// "--generation N" before the agent ID prints the token of that generation,
// which a credential reset moves an agent to; without it, or with 0, the token
// is the one podium agent-token printed before generations existed, the token
// an agent enrolls with. "--generation=N" is the same. A generation that is not
// a whole number from 0 to 4294967295, or one with no value, is a usage error:
// exit 2, one line on stderr, no token.
func TestAgentTokenCLIGeneration(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no generation", []string{"agent-1"}, "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058"},
		{"generation 0", []string{"--generation", "0", "agent-1"}, "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058"},
		{"generation 1", []string{"--generation", "1", "agent-1"}, "7f6129ad9f0126964aafb88a75a2019467059c1ae9d20b931b465d3aee996fae"},
		{"generation=2", []string{"--generation=2", "agent-1"}, "089a433310152c9aa06043ed2151343c55f47fa0da583cfcbd14d149070e51e0"},
		{"generation 4294967295", []string{"--generation", "4294967295", "agent-1"}, "d7c3f7836e2a72c6dcd036fc0649a2ec86367eb8991fa1b3185e55e7eb0106e0"},
		{"generation then --", []string{"--generation", "1", "--", "agent-1"}, "7f6129ad9f0126964aafb88a75a2019467059c1ae9d20b931b465d3aee996fae"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, enrollSecret, false, tc.args...)
			if code != 0 || stdout != tc.want+"\n" || stderr != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and stdout %q", code, stdout, stderr, tc.want+"\n")
			}
		})
	}
	for _, args := range [][]string{
		{"--generation"},
		{"--generation", "agent-1"},
		{"--generation=", "agent-1"},
		{"--generation", "-1", "agent-1"},
		{"--generation", "4294967296", "agent-1"},
		{"--generation", "1.5", "agent-1"},
		{"--generation", "0x1", "agent-1"},
		{"agent-1", "--generation", "1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runAgentToken(t, enrollSecret, false, args...)
			if code != 2 || stdout != "" || !strings.HasSuffix(stderr, "\n") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 2, no stdout and one line on stderr", code, stdout, stderr)
			}
		})
	}
}

// failingWriter fails every write, as a closed or full stdout does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A token that could not be written is no success: a script that redirects the
// output would otherwise provision an agent with an empty token.
func TestAgentTokenCommandFailsWhenTheTokenCannotBeWritten(t *testing.T) {
	t.Setenv("SOLTI_AGENT_AUTH_ENROLL_SECRET", enrollSecret)
	var stderr bytes.Buffer
	if code := agentToken([]string{"agent-1"}, failingWriter{}, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Count(stderr.String(), "\n") != 1 || strings.Contains(stderr.String(), enrollSecret) {
		t.Fatalf("stderr %q, want one line without the secret", stderr.String())
	}
}

// The subcommand runs before the configuration loads: it needs none of the
// settings the server does, and a broken config file cannot stop it. The child
// is this test binary running the real main with those arguments.
func TestAgentTokenSubcommandRunsBeforeConfigLoading(t *testing.T) {
	const childEnv = "SOLTI_AGENT_TOKEN_MAIN_CHILD"
	if os.Getenv(childEnv) != "" {
		os.Args = []string{"podium", "agent-token", "agent-1"}
		main() // exits the process
		return
	}
	// The deadline only turns a child that hangs into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentTokenSubcommandRunsBeforeConfigLoading$")
	command.Env = append(os.Environ(),
		childEnv+"=1",
		"SOLTI_AGENT_AUTH_ENROLL_SECRET="+enrollSecret,
		"CONFIG_PATH="+filepath.Join(t.TempDir(), "missing.yaml"), // loading it would fail
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("podium agent-token: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if want := "ee91297d059304dd88e1c5650d195795e3fa9c921c419a61aced53e41a3c0058\n"; stdout.String() != want {
		t.Fatalf("stdout %q, want %q", stdout.String(), want)
	}
}
