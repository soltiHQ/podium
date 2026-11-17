// Package tlsconf builds *tls.Config for the control-plane's servers and its outbound agent client from file-based PEM configuration.
//
//   - ServerConfig - the CP's serving identity (cert+key), optionally requiring client certificates (mTLS) via a client CA bundle.
//   - ClientConfig - the CP acting as a client to agents: a CA bundle to verify agent certificates, optionally presenting a client cert (mTLS).
//   - ForwardConfig - a follower forwarding a write to the leader: a CA bundle and a server name that verify the leader's certificate
//     in place of tls.client's CA and the leader's host; the client certificate stays tls.client's.
//
// The Build methods return (nil, nil) when TLS is not configured;
// callers transparently fall back to plaintext - TLS is opt-in.
package tlsconf
