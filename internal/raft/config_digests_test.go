package raft

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// testDigests returns configuration digests that differ for every seed: each
// byte of group i is seed+i.
func testDigests(seed byte) cluster.Digests {
	var d cluster.Digests
	for i := range d {
		for j := range d[i] {
			d[i][j] = seed + byte(i)
		}
	}
	return d
}

// withConfigDigests gives a node d as its configuration digests.
func withConfigDigests(d cluster.Digests) func(*Config) {
	return func(c *Config) { c.ConfigDigests = &d }
}

// memberTLS is the raft TLS of a peer that presents a certificate of ca for
// 127.0.0.1, as a member of ca's cluster does.
func memberTLS(t *testing.T, ca *testCA) *transportTLS {
	t.Helper()
	return &transportTLS{client: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      ca.pool(),
		Certificates: []tls.Certificate{certOf(t, ca.issue(t, "127.0.0.1"))},
	}}
}

// strangerHeader is the header of the RPCs of a peer in no configuration.
var strangerHeader = hraft.RPCHeader{ProtocolVersion: hraft.ProtocolVersionMax, ID: []byte("intruder"), Addr: []byte("intruder")}

// everyRPC sends one RPC of each type of hashicorp/raft v1.8.0 through trans to
// addr, at term 0, which a node answers without taking it, and returns the error
// of the first that fails, named. TimeoutNow comes last: it makes the node start
// an election, which a node of one wins again.
func everyRPC(trans *hraft.NetworkTransport, addr string) error {
	target := hraft.ServerAddress(addr)
	for _, rpc := range []struct {
		name string
		send func() error
	}{
		{"AppendEntries", func() error {
			return trans.AppendEntries("intruder", target, &hraft.AppendEntriesRequest{RPCHeader: strangerHeader}, &hraft.AppendEntriesResponse{})
		}},
		{"RequestVote", func() error {
			return trans.RequestVote("intruder", target, &hraft.RequestVoteRequest{RPCHeader: strangerHeader}, &hraft.RequestVoteResponse{})
		}},
		{"RequestPreVote", func() error {
			return trans.RequestPreVote("intruder", target, &hraft.RequestPreVoteRequest{RPCHeader: strangerHeader}, &hraft.RequestPreVoteResponse{})
		}},
		{"InstallSnapshot", func() error {
			req := &hraft.InstallSnapshotRequest{RPCHeader: strangerHeader, SnapshotVersion: hraft.SnapshotVersionMax}
			return trans.InstallSnapshot("intruder", target, req, &hraft.InstallSnapshotResponse{}, bytes.NewReader(nil))
		}},
		{"TimeoutNow", func() error {
			return trans.TimeoutNow("intruder", target, &hraft.TimeoutNowRequest{RPCHeader: strangerHeader}, &hraft.TimeoutNowResponse{})
		}},
	} {
		if err := rpc.send(); err != nil {
			return fmt.Errorf("%s: %w", rpc.name, err)
		}
	}
	return nil
}

// Only the first byte of a connection can be a configuration request: a
// connection whose first read starts with another byte passes it and every
// later byte through untouched, configRequest at the start of a later read
// included, while one whose first byte is configRequest writes the answer,
// closes, and reads the end of the stream.
func TestOnlyTheFirstByteIsAConfigRequest(t *testing.T) {
	own := testDigests(3)
	answer := configAnswer(&own)
	read := func(c *answerConn) ([]byte, error) {
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		return buf[:n], err
	}

	t.Run("after another first byte", func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		conn := &answerConn{Conn: server, answer: answer}
		defer func() { _ = conn.Close() }()
		for _, chunk := range [][]byte{{0x01, 0x02}, {configRequest, 0x03}} {
			go func() { _, _ = client.Write(chunk) }()
			got, err := read(conn)
			if err != nil || !bytes.Equal(got, chunk) {
				t.Fatalf("read %x, %v, want %x passed through", got, err, chunk)
			}
		}
	})

	t.Run("as the first byte", func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		conn := &answerConn{Conn: server, answer: answer}
		got := make(chan []byte, 1)
		go func() {
			_, _ = client.Write([]byte{configRequest})
			b, _ := io.ReadAll(client)
			got <- b
		}()
		if b, err := read(conn); len(b) != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read %x, %v, want the end of the stream", b, err)
		}
		if b := <-got; !bytes.Equal(b, answer) {
			t.Fatalf("the peer read %x, want the answer %x", b, answer)
		}
		if d, err := decodeConfigAnswer(answer); err != nil || d != own {
			t.Fatalf("the answer decodes as %x, %v, want %x", d, err, own)
		}
	})
}

// The answer has a fixed format, version 1: the version byte, then the
// digests of the five groups in their order, 32 bytes each, 161 bytes in all.
// A fetch refuses an answer of another version or size, and an empty one says
// that the peer serves none.
func TestConfigAnswerFormat(t *testing.T) {
	own := testDigests(5)
	answer := configAnswer(&own)
	if len(answer) != 161 || answer[0] != 1 {
		t.Fatalf("answer of %d bytes, version %d, want 161 bytes of version 1", len(answer), answer[0])
	}
	for i, g := range own {
		if at := 1 + 32*i; !bytes.Equal(answer[at:at+32], g[:]) {
			t.Fatalf("answer bytes %d to %d are %x, want the digest of group %d, %x", at, at+32, answer[at:at+32], i, g)
		}
	}
	if configAnswer(nil) != nil {
		t.Fatal("a node without digests has an answer")
	}
	for _, tc := range []struct {
		name   string
		answer []byte
	}{
		{"empty", nil},
		{"of version 2", append([]byte{2}, answer[1:]...)},
		{"short", answer[:len(answer)-1]},
		{"long", append(slices.Clone(answer), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := decodeConfigAnswer(tc.answer)
			if err == nil || d != (cluster.Digests{}) {
				t.Fatalf("decodeConfigAnswer(%x) = %x, %v, want an error", tc.answer, d, err)
			}
			if empty := len(tc.answer) == 0; empty != errors.Is(err, errNoConfigAnswer) {
				t.Fatalf("decodeConfigAnswer(%x) = %v; %v only for an empty answer", tc.answer, err, errNoConfigAnswer)
			}
		})
	}
}

// A node answers a configuration request on its raft port with its own
// digests, to a peer that presents a certificate of the cluster CA, and logs
// no error for it. The answer leaves raft alone: an RPC of every type of
// hashicorp/raft still reaches raft and is answered, as the first RPC of a
// connection and as a later one, the probe of a membership change still ends
// in a quiet end of stream, and the next request is answered again. A peer with
// a certificate of another CA, with none, over TLS 1.2 or over plain TCP gets
// no answer.
func TestRaftPortAnswersConfigRequests(t *testing.T) {
	c := loopbackNodes(t, "n1")
	node := c.nodes[0]
	logs := node.capture()
	own := testDigests(1)
	node.start(t, node.self(), oneNodeTimeouts, withConfigDigests(own))
	waitFor(t, 5*time.Second, "n1 leads", node.profile.leadership.AmLeader)
	ctx := context.Background()
	member := memberTLS(t, c.ca)

	if got, err := fetchDigests(ctx, node.addr, member); err != nil || got != own {
		t.Fatalf("fetch from %s: %x, %v, want its digests %x", node.id, got, err, own)
	}
	trans := peerTransport(t, member.client)
	for round := range 2 {
		if err := everyRPC(trans, node.addr); err != nil {
			t.Fatalf("round %d of raft RPCs to a node that answers configuration requests: %v", round+1, err)
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := probe(probeCtx, node.addr, member); err != nil {
		t.Fatalf("probe of %s: %v", node.id, err)
	}
	if got, err := fetchDigests(ctx, node.addr, member); err != nil || got != own {
		t.Fatalf("second fetch from %s: %x, %v, want its digests %x", node.id, got, err, own)
	}
	if out := logs.String(); strings.Contains(out, `"level":"error"`) {
		t.Fatalf("%s logged an error for a configuration request, an RPC or the probe: %s", node.id, out)
	}

	var (
		pool     = c.ca.pool()
		stranger = certOf(t, newTestCA(t).issue(t, "127.0.0.1"))
	)
	for _, tc := range []struct {
		name     string
		security *transportTLS // nil is plain TCP
	}{
		{"a certificate of another CA", &transportTLS{client: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{stranger}}}},
		{"no certificate", &transportTLS{client: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}},
		{"TLS 1.2", &transportTLS{client: &tls.Config{MaxVersion: tls.VersionTLS12, RootCAs: pool, Certificates: member.client.Certificates}}},
		{"plain TCP", nil},
	} {
		t.Run("from a peer with "+tc.name, func(t *testing.T) {
			// Over plain TCP the node waits for the rest of a TLS record after
			// the request's one byte, until the fetch gives up: a shorter bound
			// than the fetch's own 5s ends the wait sooner.
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			got, err := fetchDigests(ctx, node.addr, tc.security)
			if err == nil || got != (cluster.Digests{}) {
				t.Fatalf("fetch from %s: %x, %v, want no answer", node.id, got, err)
			}
		})
	}
}

// On plain TCP, with allow_insecure_transport, a node answers a configuration
// request as openly as the rest of its raft port: any host that reaches it gets
// the digests, without a certificate. RPCs of every type still reach raft, and
// the probe still ends in a quiet end of stream.
func TestPlainTCPAnswersConfigRequests(t *testing.T) {
	c := loopbackNodes(t, "n1")
	node := c.nodes[0]
	logs := node.capture()
	own := testDigests(7)
	node.start(t, node.self(), oneNodeTimeouts, insecureTransport, withConfigDigests(own))
	waitFor(t, 5*time.Second, "n1 leads", node.profile.leadership.AmLeader)
	ctx := context.Background()

	if got, err := fetchDigests(ctx, node.addr, nil); err != nil || got != own {
		t.Fatalf("plain TCP fetch from %s: %x, %v, want its digests %x", node.id, got, err, own)
	}
	if err := everyRPC(peerTransport(t, nil), node.addr); err != nil {
		t.Fatalf("plain TCP RPCs to a node that answers configuration requests: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := probe(probeCtx, node.addr, nil); err != nil {
		t.Fatalf("plain TCP probe of %s: %v", node.id, err)
	}
	if out := logs.String(); strings.Contains(out, `"level":"error"`) {
		t.Fatalf("%s logged an error: %s", node.id, out)
	}
}

// A node without configuration digests answers no configuration request: the
// byte goes to hashicorp/raft, which refuses it as an RPC type it does not know
// with one ERROR line and closes the connection, as a version of podium without
// the check does. So a peer that asks a node of such a version, during a
// rolling upgrade, gets errNoConfigAnswer, and the node's raft is untouched.
func TestNodeWithoutDigestsAnswersNoConfigRequest(t *testing.T) {
	c := loopbackNodes(t, "n1")
	node := c.nodes[0]
	logs := node.capture()
	node.start(t, node.self(), oneNodeTimeouts)
	waitFor(t, 5*time.Second, "n1 leads", node.profile.leadership.AmLeader)
	member := memberTLS(t, c.ca)

	if got, err := fetchDigests(context.Background(), node.addr, member); !errors.Is(err, errNoConfigAnswer) || got != (cluster.Digests{}) {
		t.Fatalf("fetch from a node without digests: %x, %v, want %v", got, err, errNoConfigAnswer)
	}
	waitFor(t, 5*time.Second, "n1 refuses the request as an unknown RPC type", func() bool {
		return strings.Contains(logs.String(), fmt.Sprintf("unknown rpc type %d", configRequest))
	})
	if err := everyRPC(peerTransport(t, member.client), node.addr); err != nil {
		t.Fatalf("RPCs to a node without digests: %v", err)
	}
}
