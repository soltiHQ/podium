package cluster

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// fingerprintVersion names the canonical encoding of [SharedSettings], and is
// the message every secret is digested with: a secret enters its group's digest
// as HMAC-SHA256(secret, fingerprintVersion), never as itself.
const fingerprintVersion = "podium-config-fingerprint-v1"

// The groups of [SharedSettings], by the names logs give them, in the order of
// [Digests].
const (
	GroupAuth      = "auth"       // auth.jwt_secret, auth.cookie_secure
	GroupAgentAuth = "agent auth" // agent_auth.require, agent_auth.enroll_secret
	GroupProxies   = "proxies"    // trusted_proxies
	GroupCORS      = "CORS"       // cors.*
)

// GroupCount is the number of groups of [SharedSettings].
const GroupCount = 4

// groupNames are the groups of [Digests], in order.
var groupNames = [GroupCount]string{GroupAuth, GroupAgentAuth, GroupProxies, GroupCORS}

// SharedSettings are the settings that every replica of a cluster must share,
// as a node applies them. A replica whose own differ from its leader's
// authenticates, trusts or answers otherwise than the leader: a token the
// leader signed fails on it, a proxy the leader trusts is a client on it, a
// browser origin the leader allows is refused on it.
//
// The caller gives each setting as the component that applies it reads it:
// TrustedProxies as the client IP resolver parses them, and CORS with the
// defaults of the CORS middleware filled in. Digests then encodes each group
// canonically, so that settings that apply alike encode alike (see Digests).
type SharedSettings struct {
	// JWTSecret and CookieSecure are auth.jwt_secret and auth.cookie_secure.
	JWTSecret    string
	CookieSecure string
	// AgentAuthRequire and EnrollSecret are agent_auth.require and
	// agent_auth.enroll_secret.
	AgentAuthRequire bool
	EnrollSecret     string
	// TrustedProxies are the prefixes of trusted_proxies, as the client IP
	// resolver parses them, in any order.
	TrustedProxies []netip.Prefix
	// CORS is cors.*, with the CORS middleware's defaults filled in.
	CORS CORSSettings
}

// CORSSettings are the settings of the CORS middleware, cors.*, with its
// defaults filled in.
type CORSSettings struct {
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           time.Duration
}

// Digests are the digests of the groups of a node's [SharedSettings], in the
// order auth, agent auth, proxies, CORS: each the SHA-256 of its group's
// canonical encoding. They travel between nodes only; no log line and no
// answer of the API holds them.
type Digests [GroupCount][sha256.Size]byte

// Digests returns the digests of s, group by group. Each digest is the SHA-256
// of an encoding that starts with fingerprintVersion and the group's name and
// writes each setting in a fixed order, every string and list prefixed with
// its length, so that no two different groups of settings encode alike. Each
// setting is encoded as it applies:
//
//   - a secret, auth.jwt_secret and agent_auth.enroll_secret, as
//     HMAC-SHA256(secret, "podium-config-fingerprint-v1"), an unset one
//     included, never as itself;
//   - auth.cookie_secure as written, "auto" or "always", the two values
//     validation allows, and agent_auth.require and cors.allow_credentials as
//     booleans;
//   - trusted_proxies as a set of the prefixes the resolver trusts, sorted,
//     without duplicates: overlapping or adjacent prefixes are not merged;
//   - cors.allow_origins as a set of origins as written, which CORS compares
//     exactly with a request's Origin;
//   - cors.allow_methods, cors.allow_headers and cors.expose_headers as the
//     sets of names their header lists carry, as a browser reads them: the
//     entries joined with commas, split on commas and trimmed, empty names
//     dropped, header names in lower case, as they compare without case,
//     methods as written, as they compare with case;
//   - cors.max_age in the whole seconds that CORS sends.
func (s SharedSettings) Digests() Digests {
	c := s.CORS
	return Digests{
		digest(GroupAuth, func(e encoder) {
			e.bytes(secretMAC(s.JWTSecret))
			e.string(s.CookieSecure)
		}),
		digest(GroupAgentAuth, func(e encoder) {
			e.bool(s.AgentAuthRequire)
			e.bytes(secretMAC(s.EnrollSecret))
		}),
		digest(GroupProxies, func(e encoder) {
			proxies := make([]string, 0, len(s.TrustedProxies))
			for _, p := range s.TrustedProxies {
				proxies = append(proxies, p.String())
			}
			e.strings(set(proxies))
		}),
		digest(GroupCORS, func(e encoder) {
			e.strings(set(slices.Clone(c.AllowOrigins)))
			e.strings(headerNames(c.AllowMethods, false))
			e.strings(headerNames(c.AllowHeaders, true))
			e.strings(headerNames(c.ExposeHeaders, true))
			e.bool(c.AllowCredentials)
			e.int(int64(c.MaxAge.Seconds()))
		}),
	}
}

// Fingerprint is the fingerprint of s, the settings every replica must share:
// the hex SHA-256 over the digests of its groups, in order. Nodes whose
// fingerprints differ do not share them.
func Fingerprint(s SharedSettings) string {
	return s.Digests().Fingerprint()
}

// Fingerprint is the hex SHA-256 over d, its groups' digests in order.
func (d Digests) Fingerprint() string {
	h := sha256.New()
	for _, g := range d {
		_, _ = h.Write(g[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Differ names the groups whose digests differ between d and other, in order,
// by the names of the Group constants; it names none when every group matches.
func (d Digests) Differ(other Digests) []string {
	var out []string
	for i := range d {
		if d[i] != other[i] {
			out = append(out, groupNames[i])
		}
	}
	return out
}

// digest is the SHA-256 of the encoding of the group name that write writes,
// after fingerprintVersion and the name.
func digest(name string, write func(encoder)) [sha256.Size]byte {
	e := encoder{h: sha256.New()}
	e.string(fingerprintVersion)
	e.string(name)
	write(e)
	var sum [sha256.Size]byte
	copy(sum[:], e.h.Sum(nil))
	return sum
}

// secretMAC is HMAC-SHA256(secret, fingerprintVersion).
func secretMAC(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(fingerprintVersion))
	return mac.Sum(nil)
}

// set sorts list in place and drops its duplicates.
func set(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// headerNames returns the set of names of the header list that CORS sends for
// list: the entries joined with commas, then split on them, each name trimmed
// of spaces and tabs, empty names dropped, in lower case when fold is set.
func headerNames(list []string, fold bool) []string {
	var names []string
	for _, name := range strings.Split(strings.Join(list, ","), ",") {
		name = strings.Trim(name, " \t")
		if name == "" {
			continue
		}
		if fold {
			name = strings.ToLower(name)
		}
		names = append(names, name)
	}
	return set(names)
}

// encoder writes values into h so that every sequence of values has its own
// encoding: an integer as 8 bytes, big endian, a boolean as a byte, 0 or 1,
// and a byte string, a string or a list after its length.
type encoder struct{ h hash.Hash }

func (e encoder) uint(n uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	_, _ = e.h.Write(b[:])
}

func (e encoder) int(n int64) { e.uint(uint64(n)) }

func (e encoder) bool(v bool) {
	b := byte(0)
	if v {
		b = 1
	}
	_, _ = e.h.Write([]byte{b})
}

func (e encoder) bytes(b []byte) {
	e.uint(uint64(len(b)))
	_, _ = e.h.Write(b)
}

func (e encoder) string(s string) { e.bytes([]byte(s)) }

func (e encoder) strings(list []string) {
	e.uint(uint64(len(list)))
	for _, s := range list {
		e.string(s)
	}
}
