// Package sync implements a server.Runner that reconciles Rollout records against the live state on agents:
//  1. Lists due rollouts: all but a Failed one still in its backoff or out of retries.
//  2. Groups them by agent. One worker per agent dispatches them by [enum.RolloutIntent]: uninstalls that need
//     no call first, then pushes, ordered by last failure (rollouts that never failed first, then the oldest
//     failure), then the observations, all in one listing of the agent's tasks. A turn starts calls for
//     PushTimeout of wall time at most; the rest waits for the next tick. The first call the agent leaves
//     unanswered ends its turn and backs the agent off; until the backoff passes, or the agent row shows a
//     reconnect, only uninstalls that need no call run.
//     - Install    → ApplyTask(spec)                               → save TaskId
//     - Update     → ApplyTask(spec)                               → save new TaskId
//     - Uninstall  → DeleteTask(actualTaskID) (or noop if empty)   → drop rollout
//     - Noop       → ListTasks (every page, once per agent)        → reinstall what the agent lost
//     A page starts at 1000 tasks. One that the proxy refuses as too large is asked again from the same cursor
//     with half the limit, down to one task. Only a complete listing shows a task missing: if the turn's
//     budget runs out, a task is too large to list even alone, a page fails or the agent repeats a cursor, no
//     rollout is judged and the next tick lists again from the first page, at 1000. So every synced rollout of
//     an agent is observed each tick when its task list can be listed within PushTimeout. The runner logs when
//     that stops being so, with the cause, and when it is so again, not on every tick.
//  3. After processing rollouts, the finalizer pass actually removes any
//     `Spec.DeletionRequested=true` spec whose last rollout has drained.
package sync
