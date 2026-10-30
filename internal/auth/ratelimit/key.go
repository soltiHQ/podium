package ratelimit

import (
	"net/netip"

	"github.com/soltiHQ/control-plane/domain/model"
)

// AccountKey is the login key of an account: "login:acct:<subject>".
// The subject is normalized the way user storage normalizes it.
func AccountKey(subject string) string {
	return "login:acct:" + model.NormalizeSubject(subject)
}

// DeviceKey is the login key of a known device, a browser with a valid device
// cookie: "login:dev:<deviceID>".
func DeviceKey(deviceID string) string {
	return "login:dev:" + deviceID
}

// LoginIPKey is the login key of a client IP: "login:ip:<network>".
func LoginIPKey(ip string) string {
	return "login:ip:" + network(ip)
}

// AgentKey is the discovery key of an agent ID synced from a client IP: "agent:<id>@<network>".
func AgentKey(ip, agentID string) string {
	return "agent:" + agentID + "@" + network(ip)
}

// network is the part of a client IP that per-IP keys count: the address for
// IPv4 (IPv4-mapped addresses are unmapped) and the /64 prefix for IPv6, since
// one host usually controls a whole /64. Input that is not an IP is kept as is.
func network(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64) // cannot fail: an IPv6 address has 128 bits
	return p.String()
}
