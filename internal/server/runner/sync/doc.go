// Package sync implements a server.Runner that reconciles Rollout records against the live state on agents:
//  1. Lists due rollouts: all but a Failed one still in its backoff or out of retries.
//  2. Groups them by agent. One worker per agent dispatches them by [enum.RolloutIntent]: uninstalls that need
//     no call first, then pushes, then observations. The first call the agent leaves unanswered ends its turn
//     and backs the agent off; until the backoff passes, only uninstalls that need no call run.
//     - Install    → ApplyTask(spec)                               → save TaskId
//     - Update     → ApplyTask(spec)                               → save new TaskId
//     - Uninstall  → DeleteTask(actualTaskID) (or noop if empty)   → drop rollout
//     - Noop       → GetTask(actualTaskID)                         → reinstall if the agent lost it
//  3. After processing rollouts, the finalizer pass actually removes any
//     `Spec.DeletionRequested=true` spec whose last rollout has drained.
package sync
