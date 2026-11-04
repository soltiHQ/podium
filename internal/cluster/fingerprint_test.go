package cluster_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// published returns every form in which d and its fingerprint could reach a
// reader: the fingerprint, and each digest raw, as hex and as base64.
func published(d cluster.Digests) []string {
	out := []string{d.Fingerprint()}
	for _, g := range d {
		out = append(out, string(g[:]), hex.EncodeToString(g[:]), base64.StdEncoding.EncodeToString(g[:]))
	}
	return out
}

// revealing returns the forms of secret that must not appear in what is
// published: the secret itself, as hex and as base64, and its plain SHA-256,
// raw, as hex and as base64.
func revealing(secret string) []string {
	sum := sha256.Sum256([]byte(secret))
	return []string{
		secret, hex.EncodeToString([]byte(secret)), base64.StdEncoding.EncodeToString([]byte(secret)),
		string(sum[:]), hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:]),
	}
}

// A secret enters its group's digest only as HMAC-SHA256(secret,
// "podium-config-fingerprint-v1"), so neither the fingerprint nor any digest
// holds either secret or the plain SHA-256 of either, in any form, raw, as hex
// or as base64. A change of a secret still changes its group's digest, and the
// fingerprint, and no other group's digest, a change of one byte included.
func TestFingerprintRevealsNoSecret(t *testing.T) {
	const (
		jwtSecret    = "jwt-signing-secret-of-the-fingerprint-test"
		enrollSecret = "enrollment-secret-of-the-fingerprint-test"
	)
	settings := cluster.SharedSettings{JWTSecret: jwtSecret, CookieSecure: "auto", AgentAuthRequire: true, EnrollSecret: enrollSecret}
	digests := settings.Digests()
	if fp := cluster.Fingerprint(settings); fp != digests.Fingerprint() || len(fp) != 2*sha256.Size {
		t.Fatalf("Fingerprint() = %q, want the hex SHA-256 over the digests, %q", fp, digests.Fingerprint())
	}
	for _, secret := range []string{jwtSecret, enrollSecret} {
		for _, form := range revealing(secret) {
			for _, out := range published(digests) {
				if strings.Contains(out, form) {
					t.Fatalf("a published form %x holds the secret %q as %x", out, secret, form)
				}
			}
		}
	}

	for _, tc := range []struct {
		name  string
		group string
		edit  func(*cluster.SharedSettings)
	}{
		{"another jwt_secret", cluster.GroupAuth, func(s *cluster.SharedSettings) { s.JWTSecret = "another-signing-secret-of-the-fingerprint-test" }},
		{"a jwt_secret one byte apart", cluster.GroupAuth, func(s *cluster.SharedSettings) { s.JWTSecret = jwtSecret[:len(jwtSecret)-1] + "x" }},
		{"another enroll_secret", cluster.GroupAgentAuth, func(s *cluster.SharedSettings) { s.EnrollSecret = "another-enrollment-secret-of-the-test" }},
		{"no enroll_secret", cluster.GroupAgentAuth, func(s *cluster.SharedSettings) { s.EnrollSecret = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := settings
			tc.edit(&changed)
			if got := changed.Digests().Fingerprint(); got == digests.Fingerprint() {
				t.Fatalf("%s leaves the fingerprint %s", tc.name, got)
			}
			if got := digests.Differ(changed.Digests()); !slices.Equal(got, []string{tc.group}) {
				t.Fatalf("%s changes the digests of %v, want %s only", tc.name, got, tc.group)
			}
		})
	}
}

// Differ names the groups whose digests differ, in the order of the digests,
// by the names logs give them, and none when every group matches.
func TestDigestsNameTheGroupsThatDiffer(t *testing.T) {
	var a cluster.Digests
	for i := range a {
		a[i][0] = byte(i + 1)
	}
	b := a
	b[0][31] ^= 1
	b[3][7] ^= 1
	if got := a.Differ(a); len(got) != 0 {
		t.Fatalf("Differ(itself) = %v, want none", got)
	}
	if got, want := a.Differ(b), []string{cluster.GroupAuth, cluster.GroupCORS}; !slices.Equal(got, want) {
		t.Fatalf("Differ() = %v, want %v", got, want)
	}
	all := a
	for i := range all {
		all[i][1] ^= 1
	}
	want := []string{"auth", "agent auth", "proxies", "CORS", "agent TLS"}
	if got := a.Differ(all); !slices.Equal(got, want) {
		t.Fatalf("the groups are %v, want %v in that order", got, want)
	}
}

// The digests of a fixed configuration are known answers of the v1 encoding,
// which nodes of different versions compare: a change of the encoding, of the
// order of a group's settings, or of how a secret enters it, changes them. The
// values come from a separate implementation of the encoding as
// internal/raft/README.md describes it, which shares no code with this one: a
// secret enters as HMAC-SHA256(secret, "podium-config-fingerprint-v1"), and
// every string and list is prefixed with its length as 8 bytes, big endian.
func TestDigestsOfAKnownConfiguration(t *testing.T) {
	settings := cluster.SharedSettings{
		JWTSecret:        "golden-jwt-secret",
		CookieSecure:     "auto",
		Issuer:           "solti",
		Audience:         "control-plane",
		AgentAuthRequire: true,
		EnrollSecret:     "golden-enroll-secret",
		TrustedProxies:   []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("10.0.0.1/32")},
		CORS: cluster.CORSSettings{
			AllowOrigins:     []string{"https://b.example", "https://a.example"},
			AllowMethods:     []string{"POST, GET"},
			AllowHeaders:     []string{"Content-Type", "Authorization"},
			ExposeHeaders:    []string{"ETag"},
			AllowCredentials: true,
			MaxAge:           90*time.Second + 500*time.Millisecond,
		},
		AgentMTLS: true,
		AgentCAs:  [][]byte{[]byte("ca-two"), []byte("ca-one")},
	}
	want := []string{
		"7fa92fba3cee043e53e14a7ba452e8aa01fbabd95c79d5f90033185367c883c7", // auth
		"a17b1eb8179896b282f94371958cdbf1d5b5581405fa7e67ea0720748acdbb32", // agent auth
		"2c8cff09418cf0cadbdd5774154662d0c42a07e29265c8e45ba22468eb2d7178", // proxies
		"bcc31704cce027c5e6174b4c3914ec6e7a0f460bf139ef0e25be20d49184db5d", // CORS
		"e331c65a7791594f61705d87c159e5a3db8c5f9a8178280c792bd2914ab6b421", // agent TLS
	}
	const fingerprint = "e88b9d3c6f786f4bf24154e5c51a9e6691b2bc0a466423ee661712a7e924f189"
	digests := settings.Digests()
	var got []string
	for _, g := range digests {
		got = append(got, hex.EncodeToString(g[:]))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("digests\n%v\nwant\n%v", got, want)
	}
	if fp := cluster.Fingerprint(settings); fp != fingerprint {
		t.Fatalf("Fingerprint() = %s, want %s", fp, fingerprint)
	}
}
