package raft

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	hraft "github.com/hashicorp/raft"
	"github.com/rs/zerolog"

	"github.com/soltiHQ/control-plane/domain/model"
	"github.com/soltiHQ/control-plane/internal/storage"
)

// logLines returns the lines of logs as JSON objects.
func logLines(t *testing.T, logs *logBuffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("a log line that is no JSON: %q", scanner.Text())
		}
		lines = append(lines, line)
	}
	return lines
}

// hashicorp/raft logs INFO, WARN and ERROR lines at their own level and drops
// what is below INFO.
func TestHCLogAdapterKeepsLevelsFromInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  func(hclog.Logger)
		want string // the level of the line, or "" for none
	}{
		{"Trace", func(l hclog.Logger) { l.Trace("m") }, ""},
		{"Debug", func(l hclog.Logger) { l.Debug("m") }, ""},
		{"Info", func(l hclog.Logger) { l.Info("m") }, "info"},
		{"Warn", func(l hclog.Logger) { l.Warn("m") }, "warn"},
		{"Error", func(l hclog.Logger) { l.Error("m") }, "error"},
		{"Log at Trace", func(l hclog.Logger) { l.Log(hclog.Trace, "m") }, ""},
		{"Log at Debug", func(l hclog.Logger) { l.Log(hclog.Debug, "m") }, ""},
		{"Log at Info", func(l hclog.Logger) { l.Log(hclog.Info, "m") }, "info"},
		{"Log at Warn", func(l hclog.Logger) { l.Log(hclog.Warn, "m") }, "warn"},
		{"Log at Error", func(l hclog.Logger) { l.Log(hclog.Error, "m") }, "error"},
		{"Log with no level", func(l hclog.Logger) { l.Log(hclog.NoLevel, "m") }, "info"},
		{"Log at Off", func(l hclog.Logger) { l.Log(hclog.Off, "m") }, ""},
		{"standard logger", func(l hclog.Logger) { l.StandardLogger(nil).Print("m") }, "info"},
		{"standard writer at Warn", func(l hclog.Logger) {
			_, _ = io.WriteString(l.StandardWriter(&hclog.StandardLoggerOptions{ForceLevel: hclog.Warn}), "m\n")
		}, "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logBuffer{}
			tc.log(newHCLog(zerolog.New(logs), "raft"))
			lines := logLines(t, logs)
			switch {
			case tc.want == "" && len(lines) != 0:
				t.Fatalf("logged %v, want nothing", lines)
			case tc.want != "" && (len(lines) != 1 || lines[0]["level"] != tc.want || lines[0]["message"] != "m"):
				t.Fatalf("logged %v, want one %s line %q", lines, tc.want, "m")
			}
		})
	}
	l := newHCLog(zerolog.New(&logBuffer{}), "raft")
	if l.IsTrace() || l.IsDebug() || !l.IsInfo() || !l.IsWarn() || !l.IsError() || l.GetLevel() != hclog.Info {
		t.Fatal("the adapter does not report INFO as its floor")
	}
	l.SetLevel(hclog.Trace)
	if l.IsDebug() || l.GetLevel() != hclog.Info {
		t.Fatal("SetLevel moved the floor")
	}
}

// The key and value pairs of a line become the fields of an event, with the
// name of the logger as the component; With, Named and ResetNamed change them.
func TestHCLogAdapterFields(t *testing.T) {
	logs := &logBuffer{}
	l := newHCLog(zerolog.New(logs).With().Str("profile", "raft").Logger(), "raft")
	l.Info("fields",
		"text", "a", "count", 3, "term", uint64(7), "ok", true, "ratio", 0.5,
		"timeout", 250*time.Millisecond, "error", errors.New("boom"),
		"id", hraft.ServerID("n1"), "peer", hraft.Server{Suffrage: hraft.Voter, ID: "n2", Address: "10.0.0.2:7000"},
		"servers", hclog.Fmt("%+v", []string{"n1", "n2"}), "state", hraft.Follower, "nothing", nil,
		"dangling")
	l.With("peer", "n3").Named("replication").Warn("with", "index", 4)
	l.ResetNamed("snapshot").Info("reset")
	if got := l.With("a", 1).With("b", 2).ImpliedArgs(); len(got) != 4 || got[0] != "a" || got[3] != 2 {
		t.Fatalf("ImpliedArgs() = %v", got)
	}

	lines := logLines(t, logs)
	if len(lines) != 3 {
		t.Fatalf("logged %d lines, want 3: %s", len(lines), logs)
	}
	for key, want := range map[string]any{
		"component": "raft", "profile": "raft", "message": "fields",
		"text": "a", "count": 3.0, "term": 7.0, "ok": true, "ratio": 0.5,
		"timeout": "250ms", "error": "boom", "id": "n1", "peer": "{Voter n2 10.0.0.2:7000}",
		"servers": "[n1 n2]", "state": "Follower", "nothing": nil, "dangling": "EXTRA_VALUE_AT_END",
	} {
		if got, ok := lines[0][key]; !ok || got != want {
			t.Errorf("field %q = %v, want %v: %v", key, got, want, lines[0])
		}
	}
	if lines[1]["component"] != "raft.replication" || lines[1]["peer"] != "n3" || lines[1]["index"] != 4.0 || lines[1]["level"] != "warn" {
		t.Errorf("a logger with an argument and a name logged %v", lines[1])
	}
	if lines[2]["component"] != "snapshot" {
		t.Errorf("ResetNamed logged %v", lines[2])
	}
	if name := l.Named("a").Named("b").Name(); name != "raft.a.b" {
		t.Errorf("Named(a).Named(b).Name() = %q, want raft.a.b", name)
	}
}

// What a command carries never reaches the log: raft prints a request it did not
// expect whole under "command", and a byte slice is only counted.
func TestHCLogAdapterRedactsPayloads(t *testing.T) {
	const payload = "payload-4711"
	logs := &logBuffer{}
	l := newHCLog(zerolog.New(logs), "raft")
	request := &hraft.AppendEntriesRequest{Entries: []*hraft.Log{{Index: 1, Data: []byte(payload)}}}
	l.Error("expected heartbeat, got", "command", hclog.Fmt("%#v", request))
	l.Error("got unexpected command", "command", request)
	l.Info("bytes", "data", []byte(payload))
	if out := logs.String(); strings.Contains(out, payload) {
		t.Fatalf("the log holds the payload: %s", out)
	}
	lines := logLines(t, logs)
	if len(lines) != 3 || lines[0]["command"] != redacted || lines[1]["command"] != redacted || lines[2]["data"] != "[12 bytes]" {
		t.Fatalf("logged %v, want the command redacted and the bytes counted", lines)
	}
}

// captureStderr sends everything written to os.Stderr from now on to a buffer,
// and returns what stop reads from it once os.Stderr is back. The test's
// cleanup stops it too.
func captureStderr(t *testing.T) (stop func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var (
		saved   = os.Stderr
		done    = make(chan string, 1)
		once    sync.Once
		written string
	)
	os.Stderr = w
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	stop = func() string {
		once.Do(func() {
			os.Stderr = saved
			_ = w.Close()
			written = <-done
			_ = r.Close()
		})
		return written
	}
	t.Cleanup(func() { stop() })
	return stop
}

// hashicorp/raft, its snapshot store and its transport log through the node's
// logger, at their own levels, and write nothing to stderr: a snapshot is
// logged by the snapshot store, an election by raft, and a connection the
// transport refuses by the transport, at ERROR, with the node's own fields.
func TestRaftLogsGoThroughTheNodeLogger(t *testing.T) {
	stopStderr := captureStderr(t)
	c := loopbackNodes(t, "n1", "n2", "n3")
	logs := make(map[string]*logBuffer, len(c.nodes))
	for _, n := range c.nodes {
		logs[n.id] = n.capture()
		n.start(t, c.discovery(t))
	}
	leader := c.leader(t, 10*time.Second)
	agent, err := model.NewAgent("a1", "agent", "http://agent")
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, 10*time.Second, func(s storage.Storage) error { return s.UpsertAgent(context.Background(), agent) })
	if err := leader.profile.raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	// A host that is no cluster node connects and sends what is no TLS.
	conn, err := net.Dial("tcp", leader.addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	found := func(n *loopbackNode, level, component, message string) bool {
		for _, line := range logLines(t, logs[n.id]) {
			text, _ := line["message"].(string)
			if line["level"] == level && line["component"] == component && line["profile"] == "raft" && strings.HasPrefix(text, message) {
				return true
			}
		}
		return false
	}
	waitFor(t, 5*time.Second, "the leader logs the refused connection", func() bool {
		return found(leader, "error", "raft-net", "failed to decode incoming command")
	})
	for _, n := range c.nodes {
		if !found(n, "info", "raft", "entering follower state") {
			t.Errorf("%s did not log raft's INFO line about its state: %s", n.id, logs[n.id])
		}
		for _, line := range logLines(t, logs[n.id]) {
			if level := line["level"]; level == "debug" || level == "trace" {
				t.Errorf("%s logged below INFO: %v", n.id, line)
			}
		}
	}
	if !found(leader, "info", "snapshot", "creating new snapshot") {
		t.Errorf("the leader did not log its snapshot: %s", logs[leader.id])
	}
	for _, n := range c.nodes {
		n.stop()
	}
	if written := stopStderr(); written != "" {
		t.Fatalf("hashicorp/raft wrote to stderr: %s", written)
	}
}
