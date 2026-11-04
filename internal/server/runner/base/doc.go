// Package base provides a leader-gated server.Runner.
//
// It factors out the start/stop/leadership scaffolding shared by the background workers:
// invoke a tick on a fixed interval (New), or run a task once in each leadership term (NewTermTask),
// but only while this replica holds leadership;
// stay idle on followers;
// drain cleanly on shutdown.
package base
