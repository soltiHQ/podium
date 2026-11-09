// Package metrics holds podium's Prometheus metrics and serves them: one private
// registry, never the global default, with the Go and process collectors, which
// the ops listener serves at /metrics (see [Metrics.Handler]).
//
// Every name starts with podium_. A label takes one of a small fixed set of
// values: no label ever carries an agent, user, spec, task or session ID, or a
// token. The HTTP route label is the pattern of the matched mux route, or
// "other"; see [Route].
//
// A nil *[Metrics] records nothing and wraps nothing, so the code that builds
// the servers takes one without a check.
package metrics
