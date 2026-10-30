// Package devicecookie implements device cookies, the OWASP defence against
// account lockout by strangers: a browser that logged in as a subject gets a
// cookie signed for that subject, and its later login attempts on the subject
// count on a limiter key of their own instead of the account and client IP
// keys.
//
// A value reads "v1.<deviceID>.<issuedUnix>.<mac>". The device ID is 16 bytes
// from crypto/rand, base64url encoded. The mac is the base64url encoded
// HMAC-SHA256 of "v1.<deviceID>.<issuedUnix>.<subject>" under a key derived
// from the JWT secret, so a cookie is good for one subject only and cannot be
// forged without the secret.
package devicecookie

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/domain/model"
)

// MaxAge is how long a device cookie stays valid after it was issued.
const MaxAge = 90 * 24 * time.Hour

const (
	version = "v1"
	// keyLabel is what the JWT secret signs to derive the cookie key.
	keyLabel = "podium device cookie v1"
	// maxSkew is how far in the future an issue time may lie.
	maxSkew = time.Minute
	// idBytes is the size of a device ID.
	idBytes = 16
)

var encoding = base64.RawURLEncoding.Strict()

// Signer issues device cookies and checks them.
type Signer struct {
	key []byte
}

// NewSigner returns a Signer whose key is HMAC-SHA256(jwtSecret,
// "podium device cookie v1").
func NewSigner(jwtSecret []byte) *Signer {
	m := hmac.New(sha256.New, jwtSecret)
	_, _ = m.Write([]byte(keyLabel))
	return &Signer{key: m.Sum(nil)}
}

// Issue returns a new device cookie value for subject, issued at now, with a
// new device ID. The subject is normalized the way user storage normalizes it.
func (s *Signer) Issue(subject string, now time.Time) (string, error) {
	var id [idBytes]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	deviceID := encoding.EncodeToString(id[:])
	issued := strconv.FormatInt(now.Unix(), 10)
	return version + "." + deviceID + "." + issued + "." + encoding.EncodeToString(s.mac(deviceID, issued, subject)), nil
}

// Verify returns the device ID of value when value is a well-formed v1
// cookie that s signed for subject, issued at most MaxAge before now and at
// most a minute after it. Otherwise it reports false.
func (s *Signer) Verify(value, subject string, now time.Time) (string, bool) {
	parts := strings.SplitN(value, ".", 5)
	if len(parts) != 4 || parts[0] != version {
		return "", false
	}
	deviceID, issued := parts[1], parts[2]
	if id, err := encoding.DecodeString(deviceID); err != nil || len(id) != idBytes {
		return "", false
	}
	unix, err := strconv.ParseInt(issued, 10, 64)
	if err != nil {
		return "", false
	}
	if at := time.Unix(unix, 0); at.After(now.Add(maxSkew)) || now.Sub(at) > MaxAge {
		return "", false
	}
	mac, err := encoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(mac, s.mac(deviceID, issued, subject)) {
		return "", false
	}
	return deviceID, true
}

func (s *Signer) mac(deviceID, issued, subject string) []byte {
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write([]byte(version + "." + deviceID + "." + issued + "." + model.NormalizeSubject(subject)))
	return m.Sum(nil)
}
