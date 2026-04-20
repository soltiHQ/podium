// Package proxyv1 holds REST shapes the outbound proxy surfaces to the UI:
// agent task listings, task details, run history.
//
// Agent communication itself speaks canonical proto-JSON (solti.v1.* from
// api/gen/v1); this package is only the intermediate shape shown to humans.
package proxyv1
