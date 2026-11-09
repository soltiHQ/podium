// Package ops serves podium's operations listener: the liveness, readiness,
// leader and version probes of supervisors and load balancers, on a plain HTTP
// listener of its own that carries no authentication and none of the UI's
// middleware. The listener runs as an [httpserver.Runner].
package ops
