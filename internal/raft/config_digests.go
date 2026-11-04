package raft

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// configRequest is the first byte of a configuration request: a peer asks a
// node, on its raft port, for the digests of its shared settings
// ([cluster.Digests]). hashicorp/raft reads the first byte of every connection
// it accepts as the type of an RPC, and v1.8.0 numbers its types from 0 to 4
// (rpcAppendEntries, rpcRequestVote, rpcInstallSnapshot, rpcTimeoutNow and
// rpcRequestPreVote, by iota in net_transport.go). The stream layer answers a
// configuration request before hashicorp/raft reads anything (answerConn), with
// a byte far from those types, so that the types a later version adds do not
// reach it.
const configRequest byte = 0xF0

// configAnswerVersion is the first byte of the answer to a configRequest: the
// version of its format. Version 1 is that byte, then the digests of the groups
// in their order (auth, agent auth, proxies, CORS), 32 bytes each.
const configAnswerVersion byte = 1

// configAnswerSize is the size of an answer of configAnswerVersion.
const configAnswerSize = 1 + cluster.GroupCount*sha256.Size

// configFetchTimeout bounds a fetch of a peer's digests: the dial, the TLS
// handshake, the request and the answer. It also bounds the write of a node's
// answer.
const configFetchTimeout = 5 * time.Second

// errNoConfigAnswer indicates a peer that closed the connection without an
// answer: it serves no configuration digests, as a version without the
// configuration check, whose hashicorp/raft refuses the request as an RPC type
// it does not know.
var errNoConfigAnswer = errors.New("raft: the peer closed the connection without a configuration answer: it serves none, as a version without the configuration check")

// configAnswer is the answer of a node whose digests are d, nil for a node
// without digests, which answers none.
func configAnswer(d *cluster.Digests) []byte {
	if d == nil {
		return nil
	}
	answer := make([]byte, 0, configAnswerSize)
	answer = append(answer, configAnswerVersion)
	for _, g := range d {
		answer = append(answer, g[:]...)
	}
	return answer
}

// decodeConfigAnswer reads the digests of answer, which must be of
// configAnswerVersion and of its size. An empty answer is errNoConfigAnswer.
func decodeConfigAnswer(answer []byte) (cluster.Digests, error) {
	var d cluster.Digests
	switch {
	case len(answer) == 0:
		return d, errNoConfigAnswer
	case answer[0] != configAnswerVersion:
		return d, fmt.Errorf("raft: a configuration answer of version %d, want %d", answer[0], configAnswerVersion)
	case len(answer) < configAnswerSize:
		return d, fmt.Errorf("raft: a configuration answer of %d bytes, want %d", len(answer), configAnswerSize)
	case len(answer) > configAnswerSize:
		return d, fmt.Errorf("raft: a configuration answer of more than %d bytes", configAnswerSize)
	}
	for i := range d {
		copy(d[i][:], answer[1+i*sha256.Size:])
	}
	return d, nil
}

// answerConn is a connection the node accepted, over TLS or plain TCP, which
// answers a configuration request itself. On the first read that gives data,
// over TLS once the handshake verified the peer's certificate, a first byte of
// configRequest is answered: answerConn writes answer, within
// configFetchTimeout, closes the connection, and the read returns io.EOF, which
// hashicorp/raft's transport drops the connection on without a log line
// (handleConn in v1.8.0). Any other first byte, and everything after it on the
// connection, passes through untouched. A node without an answer passes a
// configuration request through too, which hashicorp/raft refuses as an RPC
// type it does not know, as a version without the check does. A request is the
// byte alone: a peer that sends more, which the node leaves unread, may see the
// connection reset before it reads the answer.
type answerConn struct {
	net.Conn
	answer []byte      // nil: the node answers no configuration request
	read   atomic.Bool // whether the first byte was read
}

func (c *answerConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n == 0 || c.read.Swap(true) || b[0] != configRequest || c.answer == nil {
		return n, err
	}
	// Nothing to report a failed answer to: the peer, which reads no answer,
	// tells it.
	_ = c.SetWriteDeadline(time.Now().Add(configFetchTimeout))
	_, _ = c.Write(c.answer)
	_ = c.Close()
	return 0, io.EOF
}

// fetchDigests asks the node at address for its digests, as probe dials it,
// within configFetchTimeout: over TLS (security), with the node's certificate
// verified against the cluster CA for the host of address as written and this
// node's certificate presented, or plain TCP when security is nil. It sends the
// request byte, reads the answer until the node closes the connection, and
// decodes it.
func fetchDigests(ctx context.Context, address string, security *transportTLS) (cluster.Digests, error) {
	ctx, cancel := context.WithTimeout(ctx, configFetchTimeout)
	defer cancel()
	conn, err := dialPeer(ctx, address, security)
	if err != nil {
		return cluster.Digests{}, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return cluster.Digests{}, err
		}
	}
	if _, err := conn.Write([]byte{configRequest}); err != nil {
		return cluster.Digests{}, err
	}
	answer, err := io.ReadAll(io.LimitReader(conn, configAnswerSize+1))
	if err != nil {
		return cluster.Digests{}, err
	}
	return decodeConfigAnswer(answer)
}
