// Package lifecycle implements a server.Runner that periodically checks agent liveness.
//
// Transitions agents through status stages: (active → inactive → disconnected → deleted)
// Thresholds are expressed as multiples of each agent's heartbeat interval;
// deletion waits for the larger of that and Config.DeleteAfter.
// Silence counts from the later of the agent's last sync and the start of this replica's leadership term,
// so neither a control plane that was down nor a replica that did not lead holds that time against its agents.
// A status only ever gets worse here: a sync is what makes an agent active again.
package lifecycle
