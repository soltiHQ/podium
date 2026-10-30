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
and opening it again takes the new token. Heartbeats, the sync state of
rollouts and completed uninstalls do not change the token. Deploy runs
unchecked, without `If-Match`, in two cases only: without AgentsGet (no panel)
and before the panel has loaded once.

The task list passes exact slot filters to the agent before pagination and uses
opaque continuation cursors. Run history is bounded by the agent's history
retention. Logs are live streams, not a durable log archive. Log stream URLs
carry the inspected task UID so reconnection cannot silently switch to a
same-name replacement. Modal identifiers are encoded as lowercase UTF-8 hex so
HTML attribute normalization preserves case-sensitive UID identity.

The log and event streams are Server-Sent Events. `http.write_timeout` does
not end a stream: before each write the stream gives it 30 seconds instead, so
a client that stops reading is dropped after 30 seconds. Each stream writes the
comment `: keepalive` after 15 seconds without output, which `EventSource`
ignores, and re-checks access every 15 seconds, whether it wrote or not: the
session must still be live and the user enabled, and the log stream also needs
AgentsGet. A failed re-check, or one that cannot be made, ends the stream, and
`EventSource` reconnects through authentication, which answers 401 or 403. A
revoked session, a disabled user or a lost AgentsGet thus stops log output
within 15 seconds. A reverse proxy in front of podium must keep its read
timeout above 15 seconds (nginx `proxy_read_timeout` defaults to 60); both
streams send `X-Accel-Buffering: no`, so nginx does not buffer them.

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
local Node. The `ui` job of the PR workflow runs the same task.

`task generate` runs `go tool templ generate`, so the generator is always the
templ version pinned in `go.mod`. `task assets` downloads Alpine and HTMX and
compiles CSS with Tailwind 4 using `static/css/app.css` as input; `task prepare`
runs both. Generated templates are required to build; the CSS and JS assets are
required for a working UI in the embedded binary.

The builder currently supports the original built-in Subprocess, Wasm and
Container workload types. Arbitrary extension workload creation and durable log
archival remain outside this UI. Stored terminal execution observations are
implemented separately from live logs.
