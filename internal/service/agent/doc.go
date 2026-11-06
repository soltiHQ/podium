// Package agent implements agent management use-cases:
//   - Paginated listing and retrieval
//   - Upsert with label and heartbeat preservation, reporting what changed
//   - Control-plane label patching
//   - Agent credentials: enrollment and the check of each sync, the token
//     presented on calls to an agent, derived from the enrollment secret or
//     stored, and the sweep that converts stored tokens the secret derives.
package agent
