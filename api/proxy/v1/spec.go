// Package proxyv1 exposes the internal REST-facing shapes produced by the
// outbound proxy. The wire format for agent communication (SubmitTask body
// and all read-side payloads) is defined by the generated genv1 types in
// api/gen/v1 and is serialized via google.golang.org/protobuf/encoding/protojson.
//
// The former CreateSpecWire / RestartWire / BackoffWire DTOs have been
// retired — agent submissions now go out as canonical proto-JSON matching
// solti.v1.CreateSpec. See internal/proxy/convert.go for the conversion.
package proxyv1
