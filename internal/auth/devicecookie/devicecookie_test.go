package devicecookie

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const testSecret = "device-cookie-test-only-jwt-secret"

// The value and its mac follow the documented format, with the key derived
// from the JWT secret and a fresh device ID on every issue.
func TestIssueFollowsTheFormat(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSigner([]byte(testSecret))
	value, err := s.Issue(" Alice ", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v1" || parts[2] != "1800000000" {
		t.Fatalf("cookie %q, want v1.<device ID>.1800000000.<mac>", value)
	}
	if id, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || len(id) != 16 {
		t.Fatalf("device ID %q is not 16 base64url bytes: %v", parts[1], err)
	}
	key := hmac.New(sha256.New, []byte(testSecret))
	_, _ = key.Write([]byte("podium device cookie v1"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	_, _ = mac.Write([]byte("v1." + parts[1] + ".1800000000.alice"))
	if want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); parts[3] != want {
		t.Fatalf("mac %q, want %q", parts[3], want)
	}

	again, err := s.Issue("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Split(again, ".")[1] == parts[1] {
		t.Fatal("two cookies share a device ID")
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSigner([]byte(testSecret))
	issue := func(s *Signer, subject string, at time.Time) string {
		t.Helper()
		value, err := s.Issue(subject, at)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	valid := issue(s, "alice", now)
	parts := strings.Split(valid, ".")
	forged := "A" + parts[3][1:]
	if forged == parts[3] {
		forged = "B" + parts[3][1:]
	}
	for _, tc := range []struct {
		name, value, subject string
		ok                   bool
	}{
		{"valid", valid, "alice", true},
		{"subject typed differently", valid, " ALICE ", true},
		{"another subject", valid, "admin", false},
		{"a minute in the future", issue(s, "alice", now.Add(time.Minute)), "alice", true},
		{"over a minute in the future", issue(s, "alice", now.Add(time.Minute+time.Second)), "alice", false},
		{"90 days old", issue(s, "alice", now.Add(-MaxAge)), "alice", true},
		{"over 90 days old", issue(s, "alice", now.Add(-MaxAge-time.Second)), "alice", false},
		{"another key", issue(NewSigner([]byte("another-test-only-jwt-signing-key")), "alice", now), "alice", false},
		{"bad mac", strings.Join([]string{parts[0], parts[1], parts[2], forged}, "."), "alice", false},
		{"wrong version", "v2." + strings.Join(parts[1:], "."), "alice", false},
		{"short device ID", strings.Join([]string{parts[0], parts[1][1:], parts[2], parts[3]}, "."), "alice", false},
		{"time not a number", strings.Join([]string{parts[0], parts[1], "soon", parts[3]}, "."), "alice", false},
		{"extra part", valid + ".x", "alice", false},
		{"garbage", "garbage", "alice", false},
		{"empty", "", "alice", false},
	} {
		deviceID, ok := s.Verify(tc.value, tc.subject, now)
		if ok != tc.ok {
			t.Errorf("%s: Verify = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && deviceID != strings.Split(tc.value, ".")[1] {
			t.Errorf("%s: device ID %q, want the one in the cookie", tc.name, deviceID)
		}
	}
}
