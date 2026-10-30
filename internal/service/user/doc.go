// Package user implements user management use-cases:
//   - Paginated listing and retrieval (by ID or subject)
//   - Upsert with field normalization and uniqueness checks
//   - Patch updates applied to the stored user inside the transaction
//   - Cascading deletion (sessions → verifiers → credentials → user)
//   - Permission ceilings: an actor grants and manages only what it holds
//   - The last enabled user with every permission is kept.
package user
