# First-start bootstrap

Bootstrap creates missing built-in roles and the missing `user-admin` identity
(subject `admin`, administrator role). It generates and logs a password only
when that user's password credential is absent. This also repairs a partially
completed first startup.

Existing roles, profile fields, disabled state, role assignments and password
credentials are preserved on every restart. A persisted administrator is never
silently re-enabled or assigned a new password by bootstrap. Retrieve the
generated password from the first-start log and manage subsequent credentials
through the authenticated account workflow.

The startup context is checked before seeding. Storage errors abort bootstrap;
they do not trigger deletion, reset or replacement of existing state. Durable
storage restores state before services and bootstrap run.

`bootstrap_test.go` covers an existing operator-managed administrator, partial
first-start repair and canceled startup.
