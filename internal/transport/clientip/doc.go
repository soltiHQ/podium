// Package clientip resolves the client IP and scheme of HTTP requests that may arrive through reverse proxies.
//
// Forwarding headers count only when the TCP peer is one of the configured trusted proxies:
//   - X-Forwarded-For - the client is the right-most entry that is not itself a trusted proxy.
//   - X-Forwarded-Proto - its first value tells whether the client used HTTPS.
//
// RFC 7239 Forwarded is not supported. Every address is returned in canonical form:
// IPv4-mapped IPv6 addresses are unmapped and zones are dropped.
package clientip
