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
//     with half the limit, down to one task. The runner also bounds what an agent can make a listing cost: a
//     page that holds more tasks than it was asked for, or names a cursor longer than 4096 bytes, is refused,
//     and a listing that would hold more than 65536 task IDs or take more than 1024 calls, refused pages
//     included, is not followed further. Only a complete listing shows a task missing: if the turn's budget
//     runs out, a task is too large to list even alone, a page fails or is refused, the listing passes a
//     bound, the agent returns a cursor it returned before, or its pages name different snapshots (resource
//     versions that differ; a page without one is let through), no rollout is judged and the next tick lists
//     again from the first page, at 1000. So every synced rollout of an agent is observed each tick when,
//     after the turn's pushes, its task list can be listed within those bounds, with the last page starting
//     inside PushTimeout. The reinstalls queued after a complete listing are local store writes, about 1 ms
//     each on the durable store, and are not bounded by PushTimeout. The runner logs when the listing stops
//     being complete, with the cause, and when it is complete again, naming the latest cause, not on every
//     tick: an agent that flips between the two logs a pair of lines for each flip.
//  3. After processing rollouts, the finalizer pass actually removes any
//     `Spec.DeletionRequested=true` spec whose last rollout has drained.
package sync
