package ratelimit

import "testing"

func TestKeyFormats(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"account subject normalized like user storage", AccountKey("  Alice\t"), "login:acct:alice"},
		{"login IPv4", LoginIPKey("203.0.113.9"), "login:ip:203.0.113.9"},
		{"login IPv6 counts the /64", LoginIPKey("2001:db8:1:2:aaaa:bbbb:cccc:dddd"), "login:ip:2001:db8:1:2::/64"},
		{"IPv4", IPKey("203.0.113.9"), "ip:203.0.113.9"},
		{"IPv4-mapped", IPKey("::ffff:203.0.113.9"), "ip:203.0.113.9"},
		{"IPv6 counts the /64", IPKey("2001:db8:1:2::1"), "ip:2001:db8:1:2::/64"},
		{"IPv6 zone dropped", IPKey("fe80::1%eth0"), "ip:fe80::/64"},
		{"not an IP", IPKey("unknown"), "ip:unknown"},
		{"agent from IPv4", AgentKey("203.0.113.9", "node-1"), "agent:node-1@203.0.113.9"},
		{"agent from IPv6", AgentKey("2001:db8:1:2::1", "node-1"), "agent:node-1@2001:db8:1:2::/64"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
