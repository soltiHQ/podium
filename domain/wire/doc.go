// Package wire holds the canonical serialisable mirrors of domain entities.
// One DTO per domain type, one layer for every transport/persistence path:
//
//   - Raft replication (gob encoding inside raft.Op)
//   - REST API shapes (ToREST helpers)
//   - Snapshot / export (future)
//
// ToDTO / FromDTO round-trip via domain constructors + persistence setters
// to reconstruct the entity byte-for-byte on the receiving side.
//
// Register wires every DTO with encoding/gob.
package wire

import (
	"encoding/gob"
	"sync"
)

var registerOnce sync.Once

// Register wires every DTO type with encoding/gob. Idempotent.
func Register() {
	registerOnce.Do(func() {
		gob.Register(&AgentDTO{})
		gob.Register(&UserDTO{})
		gob.Register(&RoleDTO{})
		gob.Register(&CredentialDTO{})
		gob.Register(&VerifierDTO{})
		gob.Register(&SessionDTO{})
		gob.Register(&SpecDTO{})
		gob.Register(&RolloutDTO{})

		// SpecDTO.KindConfig is a map[string]any — users put arbitrary
		// JSON-like nested data inside it (env vars, args arrays, etc.).
		// Gob can't walk `any` without knowing concrete types; register
		// the common ones that appear after JSON unmarshalling so the
		// encoder doesn't fail when it hits them.
		gob.Register(map[string]interface{}{})
		gob.Register([]interface{}{})
	})
}
