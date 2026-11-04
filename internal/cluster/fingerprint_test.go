package cluster_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

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
	if got, want := []string{cluster.GroupAuth, cluster.GroupAgentAuth, cluster.GroupProxies, cluster.GroupCORS}, []string{"auth", "agent auth", "proxies", "CORS"}; !slices.Equal(got, want) {
		t.Fatalf("the groups are named %v, want %v", got, want)
	}
}
