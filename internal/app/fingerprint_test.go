package app

import (
	"strings"
	"testing"
	"time"

	"github.com/soltiHQ/control-plane/internal/auth"
	"github.com/soltiHQ/control-plane/internal/cluster"
	"github.com/soltiHQ/control-plane/internal/config"
	"github.com/soltiHQ/control-plane/internal/transport/http/middleware"
)

// sharedBase is a configuration that sets every setting replicas must share.
func sharedBase() config.Config {
	cfg := config.Default()
	cfg.Auth.JWTSecret = strings.Repeat("fingerprint-test-signing-secret", 2)
	cfg.Auth.CookieSecure = auth.CookieSecureAuto
	cfg.AgentAuth.Require = true
	cfg.AgentAuth.EnrollSecret = strings.Repeat("fingerprint-test-enroll-secret", 2)
	cfg.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/16"}
	cfg.CORS = middleware.CORSConfig{
		AllowOrigins:     []string{"https://console.example", "https://admin.example"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{"X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           time.Hour,
	}
	return cfg
}

// digestsOf returns the digests of the settings of cfg that replicas must share.
func digestsOf(t *testing.T, cfg config.Config) cluster.Digests {
	t.Helper()
	shared, err := sharedSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return shared.Digests()
}

// Each setting that replicas must share counts in the fingerprint, and in the
// digest of its own group only: a change of any one changes the fingerprint and
// its group's digest, and leaves the other groups' digests as they were. No
// other setting counts. Settings that apply alike digest alike: trusted proxies
// and CORS origins in another order or listed twice, proxies written in another
// form of the same prefix, header names in another case, header lists split
// across entries otherwise, an empty CORS list and the defaults CORS applies
// for it, a max_age of zero and the 12 hours CORS applies for it, and max_ages
// in one whole second, which is what CORS sends.
func TestFingerprintChangesWithEachSetting(t *testing.T) {
	base := digestsOf(t, sharedBase())
	if base.Fingerprint() == (cluster.Digests{}).Fingerprint() {
		t.Fatal("the base configuration digests as nothing")
	}
	for _, tc := range []struct {
		setting string
		group   string // "" for a setting that does not count
		edit    func(*config.Config)
	}{
		{"auth.jwt_secret", cluster.GroupAuth, func(c *config.Config) { c.Auth.JWTSecret = strings.Repeat("another-test-signing-secret", 2) }},
		{"auth.cookie_secure", cluster.GroupAuth, func(c *config.Config) { c.Auth.CookieSecure = auth.CookieSecureAlways }},
		{"agent_auth.require", cluster.GroupAgentAuth, func(c *config.Config) { c.AgentAuth.Require = false }},
		{"agent_auth.enroll_secret", cluster.GroupAgentAuth, func(c *config.Config) { c.AgentAuth.EnrollSecret = strings.Repeat("another-test-enroll-secret", 2) }},
		{"trusted_proxies: another entry", cluster.GroupProxies, func(c *config.Config) { c.TrustedProxies = append(c.TrustedProxies, "10.0.0.2") }},
		{"trusted_proxies: a prefix of another length", cluster.GroupProxies, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/24"} }},
		{"cors.allow_origins", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowOrigins = []string{"https://console.example"} }},
		{"cors.allow_methods", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowMethods = append(c.CORS.AllowMethods, "DELETE") }},
		{"cors.allow_headers", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowHeaders = append(c.CORS.AllowHeaders, "If-Match") }},
		{"cors.expose_headers", cluster.GroupCORS, func(c *config.Config) { c.CORS.ExposeHeaders = append(c.CORS.ExposeHeaders, "ETag") }},
		{"cors.allow_credentials", cluster.GroupCORS, func(c *config.Config) { c.CORS.AllowCredentials = false }},
		{"cors.max_age", cluster.GroupCORS, func(c *config.Config) { c.CORS.MaxAge = 2 * time.Hour }},
		{"auth.issuer", "", func(c *config.Config) { c.Auth.Issuer = "another-issuer" }},
		{"auth.access_ttl", "", func(c *config.Config) { c.Auth.AccessTTL = time.Minute }},
		{"lifecycle.delete_after", "", func(c *config.Config) { c.Lifecycle.DeleteAfter = time.Hour }},
		{"cluster.raft.node_id", "", func(c *config.Config) { c.Cluster.Raft.NodeID = "n9" }},
		{"http.addr", "", func(c *config.Config) { c.HTTP.Addr = ":9090" }},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			cfg := sharedBase()
			tc.edit(&cfg)
			got := digestsOf(t, cfg)
			if tc.group == "" {
				if got != base {
					t.Fatalf("%s, which replicas need not share, changes the digests of %v", tc.setting, base.Differ(got))
				}
				return
			}
			if got.Fingerprint() == base.Fingerprint() {
				t.Fatalf("%s leaves the fingerprint as it was", tc.setting)
			}
			if differ := base.Differ(got); len(differ) != 1 || differ[0] != tc.group {
				t.Fatalf("%s changes the digests of %v, want %s only", tc.setting, differ, tc.group)
			}
		})
	}

	for _, tc := range []struct {
		name string
		a, b func(*config.Config)
	}{
		{"trusted_proxies in another order", nil, func(c *config.Config) { c.TrustedProxies = []string{"192.168.0.0/16", "10.0.0.1"} }},
		{"trusted_proxies with an entry twice", nil, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1", "192.168.0.0/16", "10.0.0.1"} }},
		{"trusted_proxies written otherwise", nil, func(c *config.Config) { c.TrustedProxies = []string{"::ffff:10.0.0.1", " 192.168.7.9/16 "} }},
		{"an IP and its /32", nil, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.1/32", "192.168.0.0/16"} }},
		{"cors.allow_origins in another order", nil, func(c *config.Config) {
			c.CORS.AllowOrigins = []string{"https://admin.example", "https://console.example"}
		}},
		{"cors.allow_origins with an origin twice", nil, func(c *config.Config) {
			c.CORS.AllowOrigins = []string{"https://console.example", "https://admin.example", "https://console.example"}
		}},
		{"cors.allow_methods in one entry, in another order", nil, func(c *config.Config) { c.CORS.AllowMethods = []string{"POST, GET"} }},
		{"cors.allow_headers in another case and order", nil, func(c *config.Config) { c.CORS.AllowHeaders = []string{"content-type", "AUTHORIZATION"} }},
		{"cors.expose_headers in another case", nil, func(c *config.Config) { c.CORS.ExposeHeaders = []string{"x-request-id"} }},
		{"cors lists empty and as the defaults", func(c *config.Config) {
			c.CORS.AllowMethods, c.CORS.AllowHeaders, c.CORS.ExposeHeaders = nil, nil, nil
		}, func(c *config.Config) {
			c.CORS.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
			c.CORS.AllowHeaders = []string{"Authorization", "Content-Type", "X-Request-Id", "If-Match"}
			c.CORS.ExposeHeaders = []string{"X-Request-Id", "ETag"}
		}},
		{"cors.max_age zero and 12 hours", func(c *config.Config) { c.CORS.MaxAge = 0 }, func(c *config.Config) { c.CORS.MaxAge = 12 * time.Hour }},
		{"cors.max_age within one second", func(c *config.Config) { c.CORS.MaxAge = 90 * time.Second }, func(c *config.Config) { c.CORS.MaxAge = 90*time.Second + 900*time.Millisecond }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := sharedBase(), sharedBase()
			for _, edit := range []struct {
				cfg  *config.Config
				edit func(*config.Config)
			}{{&a, tc.a}, {&b, tc.b}} {
				if edit.edit != nil {
					edit.edit(edit.cfg)
				}
			}
			if da, db := digestsOf(t, a), digestsOf(t, b); da != db {
				t.Fatalf("%s digest otherwise, in %v", tc.name, da.Differ(db))
			}
		})
	}
}
