# Podium UI

The UI uses server-rendered `templ` components, HTMX for read refreshes and
mutations, and Alpine for form state and dialogs. The Material-style surface,
color, radius and elevation tokens live in `static/css/app.css` and
`tailwind.config.cjs`.

## Responsibilities

- `templates/`: HTML components and typed presentation models. Permission
  controls come from `internal/uikit/policy`; every API action independently
  enforces authorization.
- `templates/jsexpr`: how a Go string enters an Alpine or HTMX expression.
  templ HTML-escapes attributes, but the browser decodes them before Alpine
  evaluates them as code, so `fetch('%s')` lets a quote in an agent ID, name or
  label inject statements. Write `fetch(%s)` with `jsexpr.String(value)`, which
  renders a double-quoted literal with quotes, `<`, `>`, `&` and line
  separators escaped. Slices and maps go through `json.Marshal`. Identifiers
  (Alpine data keys) cannot be quoted and must be compile-time constants.
  `TestNoValuePastedBetweenQuotes` fails on a quoted format verb or a value
  joined at a quote in the templates and their Go builders.
- `static/js/spec-builder.js`: spec editing state, exact argument/environment
  round trips, validation, cursor-based agent selection, and submit recovery.
  It serializes the current protobuf JSON workload shape. It also exports its
  builder for Node's built-in test runner.
- `static/js/control-plane.js`: shared HTTP failure recovery, live-update
  connection status, and task-log lifecycle. Log viewers are preserved by task
  UID across row refreshes, disconnected when removed, and retain at most
  256 KiB in the browser. An open dialog prevents a background swap from
  discarding the user's form.
- `internal/handler/api_agent_task.go`: bounded task inspection and cancellation
  requests. Inspection checks that the task and run history belong to the same
  UID. Current conditions show reconciliation independently of phase: processed
  generation equality also occurs for failed reconciliation. Open inspectors are
  preserved by agent and task UID and refresh only while expanded. Cancellation
  retains history and records a logical request; it does not
  promise immediate physical process exit.
- `internal/handler/api_spec_placement.go`: read-only saved-spec placement from the
  same planner used by Deploy. Both SpecsGet and AgentsGet are required. It
  exposes ordered runner compatibility and stored heartbeat observations; the
  plan is advisory and Deploy evaluates the current inventory again. Its plan
  token lets Deploy refuse a placement that changed since the preview.
- `internal/handler/api_history.go`: stored terminal observations independent of
  live agent/spec existence. AgentsGet protects both API and page. Exact agent,
  spec, task name, UID and terminal-result filters use opaque continuations;
  public pages default to 50 records and are capped at 100. Changing filters
  starts from the first page.
- `internal/auth/session/authorize.go`: checks durable session and current RBAC
  after JWT verification, so revoke, account disable and permission changes
  apply to the next request.

## Contract and failure behavior

Subprocess configuration uses `mode.command` or `mode.script` with an explicit
interpreter. Argument inputs use a JSON string array, preserving spaces, empty
arguments and Unicode. Environment entries preserve their order and duplicates;
selector labels reject duplicate keys. Target agents and target label matches
are combined by the control plane; all label requirements must match.

Spec saves include the loaded version. HTTP 409, validation errors and network
failures keep the draft intact. A conflict provides a link to the saved version
in another tab rather than reloading the editor. Slot remains immutable after
creation. Legacy flat command/script configurations are hydrated without losing
arguments or body. Obsolete inline-script runtimes require an explicit operator
review before switching to the current interpreter-and-file execution contract. Empty target and label selections are sent explicitly so they clear
stored selectors.

Fetch forms ask for `Accept: application/json` even when using HTMX redirect
headers. API negotiation honors that header so error messages remain readable.
A fetch that sends a JSON body declares `Content-Type: application/json`: the
API answers any other body type with 415. The server refuses mutations from
another origin with 403, and logout is a POST form in the navigation bar.
HTMX confirmation dialogs close after success and retain errors after failure.

The placement panel carries its plan token in `data-plan-token` (see
`internal/handler`, "Specs") and copies it onto its container,
`#spec-placement`, which a failed refresh empties but keeps. When the Deploy
dialog opens, it keeps the container's token, that of the plan the panel shows
or showed before a failed refresh, and sends it as `If-Match`. A placement
that changed in the meantime (a spec save, an agent that starts or stops
matching, a target that registers or changes eligibility or selected runner,
rollouts created by another deploy) is refused with 412 `Placement changed
since it was previewed. Review it again.` and nothing is written: the message
stays in the dialog and the page refreshes the panel. The panel also
refreshes while the dialog is open, so closing the dialog shows the new plan,
and opening it again takes the new token. Heartbeats and the sync state of
rollouts do not change the token, and neither does a completed uninstall of an
agent that is no longer targeted. An agent targeted again before its uninstall
completes moves from update to install when the uninstall completes, which
changes the token: one 412. Deploy runs unchecked, without `If-Match`, in two
cases only: without AgentsGet (no panel) and before the panel has loaded once.

The user edit dialog, with or without the role and permission selects, sends
the ETag of the user it was rendered from as `If-Match` (see
`internal/handler`, "Users"). An open dialog is not replaced by a background
refresh, so it keeps the tag of the user it shows. A user that changed in the
meantime (another edit, a disable or an enable) is refused with 412 `User
changed since it was read; reload and retry.` and nothing is written: the
message stays in the dialog, with a Reload link to the user's page, which
loads the current user and its tag. Reloading discards what was typed. The
create dialog sends no `If-Match`.

The agent page offers two actions next to the label editor, each only to a user
who holds its permission: "Reset credential" (`agents:reset`) and "Delete agent"
(`agents:delete`); `agents:edit` gives neither. The page does not render an
action the viewer may not use, and the API refuses it anyway (see
`internal/handler`, "Agents"). Reset credential asks for confirmation, then
posts to `/api/v1/agents/{id}/credential`. The answer is JSON, and the dialog
that follows shows the new token once, with a button that copies it and, for a
derived credential (the answer has a `generation`), the line `podium agent-token
--generation N <agent-id>`, which prints the token again with the enrollment
secret; a random token of a stored credential has no such line. The server
never renders the token into the page: it lives in the state that the two
dialogs share, and is in the page's markup only while the dialog that shows it
is open. The dialog clears it when it closes, by any means, and an open dialog
is not replaced by a background refresh. A copy button inside a dialog works
because `copy_buf.js` listens for clicks in the capture phase: a dialog stops
the clicks inside it from bubbling to the document. An answer that holds no
token reopens the confirmation with an error, since the credential is reset by
then. Delete agent asks for confirmation with a
list of what goes, the agent and every rollout of it, and what stays, its
credential and its execution history; it says that an agent that still runs
registers again at its next sync, and that resetting its credential first keeps
it out. On success the browser goes to the agents page (`HX-Redirect`).

The task list passes exact slot filters to the agent before pagination and uses
opaque continuation cursors. Run history is bounded by the agent's history
retention. Logs are live streams, not a durable log archive. Log stream URLs
carry the inspected task UID so reconnection cannot silently switch to a
same-name replacement. Modal identifiers are encoded as lowercase UTF-8 hex so
HTML attribute normalization preserves case-sensitive UID identity.

The log and event streams are Server-Sent Events. `http.write_timeout` does
not end a stream: before each write the stream gives that write 30 seconds
instead. This deadline drops a client whose write blocks, that is, whose
buffers are full. On a quiet stream the small keepalives can fit into the
buffers for a long time, so a reader that stopped reading is not guaranteed to
be dropped after 30 seconds. Each stream writes the comment `: keepalive` after
15 seconds without output, which `EventSource` ignores, and re-checks access
every 15 seconds, whether it wrote or not: the session must still be live and
the user enabled, and the log stream also needs AgentsGet. A failed re-check,
or one that cannot be made, ends the stream. After a revoked session, a
disabled user or a lost AgentsGet, the stream thus ends within one re-check
period (15 seconds) plus at most one blocked write (up to 30 seconds).
`EventSource` then reconnects through authentication, which answers 401 or 403,
or 503 if the store is unavailable. A reverse proxy in front of podium must
keep its read timeout above 15 seconds (nginx `proxy_read_timeout` defaults to
60); both streams send `X-Accel-Buffering: no`, so nginx does not buffer them.

The event stream carries refresh pulses, not data: `agent_update`,
`spec_update`, `dashboard_update`, `user_update` and `session_update`. A pulse
makes the pages that listen for it fetch again. The pages poll as well, so one
that misses a pulse is still refreshed: every 60 seconds for the agents list,
an agent's detail and its task list, the dashboard and the placement of a spec,
as `triggers` sets. Discovery sends `agent_update` only when a sync gives the
pages something new, not on every heartbeat:

- the agent is new, or its status, name, endpoint, labels or capabilities
  changed;
- the agent reconnected: it is active again after it was inactive or
  disconnected, or it reports less uptime than before, because it restarted.

A heartbeat that moves only the time of the sync and the uptime sends nothing,
so `Last heartbeat` and `Uptime` show their latest value after the next poll or
pulse. Without this, a fleet would make every open agents, dashboard and spec
page fetch again, and each spec page compute its placement again, on every
heartbeat. The sync runner sends `agent_update` too, with `spec_update`, when it
applies a push, finishes an uninstall or queues a lost task for a reinstall, so
an open agent page follows a deploy without waiting for its poll. The hub also
coalesces the pulses of a kind (`events.coalesce`, default 1 second, see
`internal/config`): the first goes out at once, and those that follow within the
window arrive as one pulse at its end, so a burst of changes refreshes a page
twice and its last change is never lost. The keepalive and the access re-check
of the streams above are not pulses and are never delayed.

Execution history shows partial capture from periodic agent polling. It can
miss attempts removed between polls and is bounded by retention policy. Each
record displays original task UID, SDK generation/attempt, source resource
version, workload API version/kind, and trusted first capture time. Source run
timestamps are separate. Historical rows have no live task actions, manifests,
commands, environment or logs. A terminal attempt can precede a retry/restart
and does not prove physical process shutdown.

## Verification

```sh
task ci/ui/test
go test ./internal/auth/... ./internal/handler ./internal/transport/http/middleware \
  ./internal/uikit/... ./domain/wire ./ui/templates/...
```

`task ci/ui/test` runs every `ui/tests/*.test.cjs` with Node's built-in test
runner in the Node container pinned in `Taskfile.yml`, so it needs Docker but no
local Node. It fails when no file matches: Node would pass with no test. The
`ui` job of the PR workflow runs the same task.

`task generate` runs `go tool templ generate`, so the generator is always the
templ version pinned in `go.mod`. `task assets` downloads Alpine and HTMX and
compiles CSS with Tailwind 4 using `static/css/app.css` as input; `task prepare`
runs both. Generated templates are required to build; the CSS and JS assets are
required for a working UI in the embedded binary.

The builder currently supports the original built-in Subprocess, Wasm and
Container workload types. Arbitrary extension workload creation and durable log
archival remain outside this UI. Stored terminal execution observations are
implemented separately from live logs.
