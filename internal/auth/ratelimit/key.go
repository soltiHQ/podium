package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

// LoginKey builds a rate-limit key for an authentication attempt over HTTP.
//
// Format: "login:<subject>:<ip>:<ua_hash>".
// Subject is lowercased and trimmed;
// Empty subject yields "login::<ip>:<ua_hash>" so that anonymous floods (no subject) are still tracked per-IP.
func LoginKey(r *http.Request, subject string) string {
	subject = strings.TrimSpace(strings.ToLower(subject))

	var (
		ip  = RemoteIP(r)
		uah = shortHash("")
	)
	if r != nil {
		uah = shortHash(r.UserAgent())
	}
	if subject == "" {
		return "login::" + ip + ":" + uah
	}
	return "login:" + subject + ":" + ip + ":" + uah
}

// IPKey builds a rate-limit key for generic per-IP throttling.
//
// Format: "ip:<addr>".
// Used by the generic HTTP middleware and gRPC interceptor to throttle requests by source address without involving authentication state.
func IPKey(addr string) string {
	return "ip:" + normalizeIP(addr)
}

// RemoteIP extracts the client IP from an HTTP request's RemoteAddr,
// handling both "host:port" and bare-IP forms. Returns "unknown" on parse
// failure — callers can rely on a non-empty string.
func RemoteIP(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		if parsed := net.ParseIP(host); parsed != nil {
			return parsed.String()
		}
	}
	if parsed := net.ParseIP(r.RemoteAddr); parsed != nil {
		return parsed.String()
	}
	return "unknown"
}

// normalizeIP strips any :port suffix and returns the canonical form of
// the IP. Unparseable input is returned verbatim.
func normalizeIP(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String()
	}
	return addr
}

// shortHash hashes s with SHA-256 and returns the first 8 bytes as hex.
// Used to fingerprint User-Agent strings compactly for composite keys.
func shortHash(s string) string {
	if s == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
