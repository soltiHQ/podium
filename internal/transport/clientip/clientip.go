package clientip

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// unknown stands for a peer address that does not parse.
const unknown = "unknown"

// ErrInvalidTrustedProxy indicates a trusted proxy entry that is neither an IP address nor a CIDR prefix.
var ErrInvalidTrustedProxy = errors.New("clientip: invalid trusted proxy")

// Resolver resolves client IPs and the request scheme. A nil *Resolver trusts no proxy.
type Resolver struct {
	trusted []netip.Prefix
}

// New returns a Resolver that trusts the forwarding headers sent by trustedProxies.
// Each entry is an IP address or a CIDR prefix; IPv4-mapped entries are unmapped.
func New(trustedProxies []string) (*Resolver, error) {
	r := &Resolver{trusted: make([]netip.Prefix, 0, len(trustedProxies))}
	for _, entry := range trustedProxies {
		p, err := parseTrusted(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("%w %q: want an IP address or a CIDR prefix", ErrInvalidTrustedProxy, entry)
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// FromAddr returns the canonical IP of an "ip", "ip:port" or "[ip]:port" address,
// such as a gRPC peer address, or "unknown" when it does not parse.
func FromAddr(addr string) string {
	a, ok := parseAddr(addr)
	if !ok {
		return unknown
	}
	return a.String()
}

// IP returns the canonical client IP of req.
//
// It is the TCP peer, unless the peer is a trusted proxy. Then it is the right-most
// X-Forwarded-For entry that is not a trusted proxy, or the left-most entry when all
// of them are. The peer stays the answer when the header is missing or empty, or
// when the walk reaches an entry that does not parse.
func (r *Resolver) IP(req *http.Request) string {
	peer, ok := parseAddr(req.RemoteAddr)
	if !ok {
		return unknown
	}
	if !r.trusts(peer) {
		return peer.String()
	}
	return r.forwardedFor(req.Header, peer).String()
}

// HTTPS reports whether the client reached the server over HTTPS: the connection
// itself is TLS, or the peer is a trusted proxy whose X-Forwarded-Proto says https.
// Only the first value of the first header line counts.
func (r *Resolver) HTTPS(req *http.Request) bool {
	if req.TLS != nil {
		return true
	}
	peer, ok := parseAddr(req.RemoteAddr)
	if !ok || !r.trusts(peer) {
		return false
	}
	proto, _, _ := strings.Cut(req.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// forwardedFor walks the X-Forwarded-For entries of every header line from the right.
// A missing or empty header is one empty entry, which does not parse.
func (r *Resolver) forwardedFor(h http.Header, peer netip.Addr) netip.Addr {
	entries := strings.Split(strings.Join(h.Values("X-Forwarded-For"), ","), ",")
	var client netip.Addr
	for i := len(entries) - 1; i >= 0; i-- {
		a, ok := parseAddr(strings.TrimSpace(entries[i]))
		if !ok {
			return peer
		}
		if !r.trusts(a) {
			return a
		}
		client = a
	}
	return client
}

func (r *Resolver) trusts(a netip.Addr) bool {
	if r == nil {
		return false
	}
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseTrusted parses one trusted proxy entry into a prefix of canonical addresses.
func parseTrusted(entry string) (netip.Prefix, error) {
	if !strings.Contains(entry, "/") {
		a, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, err
		}
		a = canonical(a)
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

// parseAddr parses an "ip", "ip:port" or "[ip]:port" address into canonical form.
func parseAddr(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return canonical(a), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return canonical(ap.Addr()), true
	}
	return netip.Addr{}, false
}

// canonical unmaps an IPv4-mapped IPv6 address and drops the zone.
func canonical(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}
