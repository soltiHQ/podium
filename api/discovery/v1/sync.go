// Package discoveryv1 is retained only to preserve the Go import path while
// the discovery HTTP DTO has been retired.
//
// Agent-facing discovery now speaks canonical proto-JSON (pbjson) matching
// solti.discover.v1.SyncRequest / SyncResponse defined in
// api/proto/v1/discovery.proto. HTTP handlers marshal/unmarshal via
// google.golang.org/protobuf/encoding/protojson against the generated
// genv1 types — see internal/handler/discovery.go.
//
// Do not add ad-hoc JSON DTOs here; extend the proto schema instead.
package discoveryv1
