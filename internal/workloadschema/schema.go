// Package workloadschema names the JSON Schema that an agent publishes for a
// workload type: its fingerprint, the lowercase hex sha256 of its RFC 8785
// canonical JSON.
package workloadschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/gowebpki/jcs"
)

// MaxSchemaBytes bounds one schema, as an agent sends it.
const MaxSchemaBytes = 64 << 10

// Fingerprint names a schema: the lowercase hex sha256 of its RFC 8785
// canonical JSON, which key order, whitespace and the spelling of numbers and
// strings do not change. It refuses raw over MaxSchemaBytes, raw that is not
// JSON, and JSON that is not an object.
func Fingerprint(raw []byte) (string, error) {
	if len(raw) > MaxSchemaBytes {
		return "", fmt.Errorf("workloadschema: schema of %d bytes is over the limit of %d", len(raw), MaxSchemaBytes)
	}
	c, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("workloadschema: schema is not valid JSON: %w", err)
	}
	if len(c) == 0 || c[0] != '{' {
		return "", errors.New("workloadschema: schema is not a JSON object")
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}
