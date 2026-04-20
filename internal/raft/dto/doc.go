// Package dto holds serialisable mirrors of domain entities used to
// transport state through Raft log entries.
//
// Each domain type has a matching DTO with all fields exported. ToDTO /
// FromDTO functions roundtrip state through domain constructors plus
// persistence setters (SetCreatedAt, SetUpdatedAt, …) so that replicas
// reconstruct the entity byte-for-byte from what the leader submitted.
//
// Encoding: standard encoding/gob. Register() wires all DTOs with gob so
// they can be carried inside [raft.Op].
package dto

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
