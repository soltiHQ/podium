package ratelimit

import "testing"

func TestKeyFormats(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"account subject normalized like user storage", AccountKey("  Alice\t"), "login:acct:alice"},
		{"known device", DeviceKey("q83vEjRWeJA"), "login:dev:q83vEjRWeJA"},
		{"login IPv4", LoginIPKey("203.0.113.9"), "login:ip:203.0.113.9"},
		{"login IPv4-mapped", LoginIPKey("::ffff:203.0.113.9"), "login:ip:203.0.113.9"},
		{"login IPv6 counts the /64", LoginIPKey("2001:db8:1:2:aaaa:bbbb:cccc:dddd"), "login:ip:2001:db8:1:2::/64"},
		{"login IPv6 zone dropped", LoginIPKey("fe80::1%eth0"), "login:ip:fe80::/64"},
		{"login not an IP", LoginIPKey("unknown"), "login:ip:unknown"},
		{"agent from IPv4", AgentKey("203.0.113.9", "node-1"), "agent:node-1@203.0.113.9"},
		{"agent from IPv6", AgentKey("2001:db8:1:2::1", "node-1"), "agent:node-1@2001:db8:1:2::/64"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
