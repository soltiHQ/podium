// Package sync implements a server.Runner that reconciles Rollout records against the live state on agents:
//  1. Lists due rollouts: all but a Failed one still in its backoff or out of retries.
//  2. Groups them by agent. One worker per agent dispatches each, in order, by its [enum.RolloutIntent],
//     and stops at the first call the agent leaves unanswered:
//     - Install    → ApplyTask(spec)                               → save TaskId
//     - Update     → ApplyTask(spec)                               → save new TaskId
//     - Uninstall  → DeleteTask(actualTaskID) (or noop if empty)   → drop rollout
//     - Noop       → GetTask(actualTaskID)                         → reinstall if the agent lost it
//  3. After processing rollouts, the finalizer pass actually removes any
//     `Spec.DeletionRequested=true` spec whose last rollout has drained.
package sync
