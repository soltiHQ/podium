package restv1

import "encoding/json"

// WorkloadType is a workload type the fleet declares (GET /api/v1/workload-types),
// with every schema fingerprint agents declare for it. APIVersion is
// "group/version", or the bare version for a type without a group.
type WorkloadType struct {
	APIVersion   string                `json:"api_version"`
	Kind         string                `json:"kind"`
	Fingerprints []WorkloadFingerprint `json:"fingerprints"`
}

// WorkloadFingerprint is one schema fingerprint of a workload type, the agents
// that declare it and whether its schema has arrived in the catalog.
type WorkloadFingerprint struct {
	Fingerprint string   `json:"fingerprint"`
	Agents      []string `json:"agents"`
	Received    bool     `json:"received"`
}

// WorkloadTypeDetail is GET /api/v1/workload-types/{group}/{version}/{kind}:
// the type with the schema of each fingerprint that has arrived.
type WorkloadTypeDetail struct {
	APIVersion   string                      `json:"api_version"`
	Kind         string                      `json:"kind"`
	Fingerprints []WorkloadFingerprintDetail `json:"fingerprints"`
}

// WorkloadFingerprintDetail is a WorkloadFingerprint with its schema, as the
// raw JSON the agent sent. The schema is absent until it has arrived. Its text
// comes from an agent: a client must not treat it as trusted.
type WorkloadFingerprintDetail struct {
	Fingerprint string          `json:"fingerprint"`
	Agents      []string        `json:"agents"`
	Received    bool            `json:"received"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}
