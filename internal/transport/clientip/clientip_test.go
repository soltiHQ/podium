package clientip

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// request builds a request that arrived from the TCP peer remoteAddr with the given header lines.
func request(remoteAddr string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for name, lines := range header {
		for _, line := range lines {
			r.Header.Add(name, line)
		}
	}
	return r
}

func mustNew(t *testing.T, trustedProxies ...string) *Resolver {
	t.Helper()
	r, err := New(trustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolverIgnoresHeadersFromUntrustedPeer(t *testing.T) {
	spoofed := http.Header{
		"X-Forwarded-For":   {"198.51.100.7"},
		"X-Forwarded-Proto": {"https"},
	}
	for _, tc := range []struct {
		name     string
		resolver *Resolver
	}{
		{"nil resolver", nil},
		{"no trusted proxies", mustNew(t)},
		{"peer outside the trusted proxies", mustNew(t, "10.0.0.0/8")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request("203.0.113.9:4711", spoofed)
			if got := tc.resolver.IP(r); got != "203.0.113.9" {
				t.Fatalf("IP = %q, want the TCP peer 203.0.113.9", got)
			}
			if tc.resolver.HTTPS(r) {
				t.Fatal("HTTPS trusted X-Forwarded-Proto from an untrusted peer")
			}
		})
	}
}

func TestResolverUsesRightmostUntrustedForwardedFor(t *testing.T) {
	resolver := mustNew(t, "10.0.0.0/8", "2001:db8:ffff::/48")
	const proxy = "10.0.0.1:443"
	for _, tc := range []struct {
		name string
		peer string
		xff  []string
		want string
	}{
		{"one proxy", proxy, []string{"198.51.100.7"}, "198.51.100.7"},
		{"proxy chain", proxy, []string{"198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		{"spoofed left-most entry", proxy, []string{"192.0.2.66, 198.51.100.7"}, "198.51.100.7"},
		{"header lines joined in order", proxy, []string{"192.0.2.66, 198.51.100.7", "10.0.0.3"}, "198.51.100.7"},
		{"entries trimmed", proxy, []string{"  198.51.100.7  ,10.0.0.2 "}, "198.51.100.7"},
		{"ip:port entry", proxy, []string{"198.51.100.7:51234"}, "198.51.100.7"},
		{"[ip]:port entry", proxy, []string{"[2001:db8::7]:51234"}, "2001:db8::7"},
		{"IPv6 chain", proxy, []string{"2001:db8::7, 2001:db8:ffff::1"}, "2001:db8::7"},
		{"IPv4-mapped entry", proxy, []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
		{"every entry trusted", proxy, []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"unparseable entry reached", proxy, []string{"198.51.100.7, unknown"}, "10.0.0.1"},
		{"unparseable entry not reached", proxy, []string{"unknown, 198.51.100.7"}, "198.51.100.7"},
		{"bracketed IP without port", proxy, []string{"[2001:db8::7]"}, "10.0.0.1"},
		{"empty header", proxy, []string{""}, "10.0.0.1"},
		{"missing header", proxy, nil, "10.0.0.1"},
		{"IPv4-mapped trusted peer", "[::ffff:10.0.0.1]:443", []string{"198.51.100.7"}, "198.51.100.7"},
		{"IPv6 trusted peer", "[2001:db8:ffff::1]:443", []string{"198.51.100.7"}, "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolver.IP(request(tc.peer, http.Header{"X-Forwarded-For": tc.xff})); got != tc.want {
				t.Fatalf("IP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolverHTTPSFromTrustedProxy(t *testing.T) {
	resolver := mustNew(t, "10.0.0.0/8")
	const proxy, direct = "10.0.0.1:443", "203.0.113.9:4711"
	for _, tc := range []struct {
		name  string
		peer  string
		tls   bool
		proto []string
		want  bool
	}{
		{"https from a trusted proxy", proxy, false, []string{"https"}, true},
		{"case-insensitive", proxy, false, []string{"HTTPS"}, true},
		{"first value trimmed", proxy, false, []string{" https , http"}, true},
		{"first value is http", proxy, false, []string{"http, https"}, false},
		{"only the first line counts", proxy, false, []string{"http", "https"}, false},
		{"http from a trusted proxy", proxy, false, []string{"http"}, false},
		{"missing header", proxy, false, nil, false},
		{"direct TLS", direct, true, nil, true},
		{"https from an untrusted peer", direct, false, []string{"https"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.peer, http.Header{"X-Forwarded-Proto": tc.proto})
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := resolver.HTTPS(r); got != tc.want {
				t.Fatalf("HTTPS = %v, want %v", got, tc.want)
			}
		})
	}
	overTLS := request(direct, nil)
	overTLS.TLS = &tls.ConnectionState{}
	if !(*Resolver)(nil).HTTPS(overTLS) {
		t.Fatal("a nil resolver ignored direct TLS")
	}
}

func TestNewValidatesTrustedProxies(t *testing.T) {
	for _, entry := range []string{"", "  ", "10.0.0.256", "10.0.0.0/33", "proxy.example.com", "10.0.0.1:80", "fe80::/64%eth0"} {
		if _, err := New([]string{"10.0.0.1", entry}); !errors.Is(err, ErrInvalidTrustedProxy) {
			t.Errorf("New(%q): %v, want ErrInvalidTrustedProxy", entry, err)
		}
	}
	resolver := mustNew(t, " 10.0.0.1 ", "192.168.0.0/16", "2001:db8::/32", "::ffff:172.16.0.1", "::ffff:172.20.0.0/112")
	for _, peer := range []string{"10.0.0.1:443", "192.168.4.4:443", "[2001:db8:1::1]:443", "172.16.0.1:443", "172.20.3.4:443"} {
		if got := resolver.IP(request(peer, http.Header{"X-Forwarded-For": {"198.51.100.7"}})); got != "198.51.100.7" {
			t.Errorf("peer %s is not trusted: IP = %q", peer, got)
		}
	}
}

func TestFromAddrCanonicalizes(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"203.0.113.9:4711", "203.0.113.9"},
		{"203.0.113.9", "203.0.113.9"},
		{"[2001:db8::1]:4711", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"[::ffff:203.0.113.9]:4711", "203.0.113.9"},
		{"[fe80::1%eth0]:4711", "fe80::1"},
		{"bufconn", "unknown"},
		{"", "unknown"},
	} {
		if got := FromAddr(tc.addr); got != tc.want {
			t.Errorf("FromAddr(%q) = %q, want %q", tc.addr, got, tc.want)
		}
		if got := (*Resolver)(nil).IP(request(tc.addr, nil)); got != tc.want {
			t.Errorf("IP from peer %q = %q, want %q", tc.addr, got, tc.want)
		}
	}
}
