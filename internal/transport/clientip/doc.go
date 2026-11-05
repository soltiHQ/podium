// Package clientip resolves the client IP and scheme of HTTP requests that may arrive through reverse proxies.
//
// Forwarding headers count only when the TCP peer is one of the configured trusted proxies:
//   - X-Forwarded-For - the client is the right-most entry that is not itself a trusted proxy.
//   - X-Forwarded-Proto - its first value tells whether the client used HTTPS.
//
// The trusted proxies are the trusted_proxies prefixes and, in raft mode, the hosts of the raft
// configuration ([Hosts]), which forward requests to the leader. A gRPC call is walked the same way
// from its x-forwarded-for metadata ([Resolver.Forwarded]).
//
// RFC 7239 Forwarded is not supported. Every address is returned in canonical form:
// IPv4-mapped IPv6 addresses are unmapped and zones are dropped.
package clientip
