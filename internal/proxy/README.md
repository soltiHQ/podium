# Agent communication

Podium calls each agent through `AgentProxy`, using the endpoint, API version
and bearer token registered by discovery. API v1 uses the current TaskService
contract in `api/proto/solti/task/v1`; discovery capabilities describe named
workload runners, their labels and supported group/version/kinds.

## Transports and resource identity

`v1_grpc.go` sends generated protobuf messages. `v1_http.go` uses the SDK CRD
JSON binding at `/apis/solti.io/v1/tasks`. Task HTTP JSON differs from protobuf
JSON: workload fields live under `workload.spec`, restart is an object and
container command is an optional array. `TaskManifestJSON` performs the explicit
mapping. Ordered environment entries and duplicate keys retain their order;
`failOnNonZero: false` stays explicit because the SDK HTTP default is true.

`SpecToProto` validates workload fields and policies before persistence or
submission. A Podium spec receives a stable DNS-compatible resource name from
its case-sensitive ID, and carries that original ID in an annotation. Applying
a manifest commits named desired state; admission and runtime cancellation
complete asynchronously through agent reconciliation.

`GetTask` and `ApplyTask` verify the returned name and UID. Delete and cancel
first resolve the current task, then pass its UID as a write precondition to
protect a replacement with the same name. Delete removes retained task state;
cancel retains desired state and attempt history. Missing tasks return
NotFound; the Podium reconciler treats that as an already completed removal.

Observed tasks preserve controller conditions separately from execution phase,
error and exit code. `generation` is desired state; `observed_generation` means
the controller processed that generation, including failed reconciliation. A
current-generation `Reconciled=False` condition can accompany `pending`, attempt
0 and no execution error. Its typed status, reason, message, generation and
transition time remain available through both Get and List transports. A stale
condition does not describe the latest desired generation; `Reconciled=True`
means the controller accepted runtime intake; admission may still queue or
reject a run, and execution success remains a separate outcome.
Condition status strings retain the SDK's `True`/`False`/`Unknown` values;
transition times use Unix milliseconds, with null only for missing HTTP data.

Task listing uses snapshot cursors (`continue`), phase/slot/label filters and a
limit up to 1000. Offset pagination is unsupported. An answer that the proxy
refuses for its size, a body over 4 MiB from an HTTP agent (an `ErrDecode` too)
or a message over the gRPC receive limit, carries `ErrResponseTooLarge`, so that
a caller can ask for a smaller page; an agent's own `ResourceExhausted`, a quota
for one, does not. What a page and an item may hold has bounds of its own, see "What
an answer may build".

The live run-history inspector joins only pages with the same
task UID and resource version. It rejects repeated cursors and histories
exceeding 1024 rows, 16 pages or 8 MiB of logical payload, returning an
actionable error instead of an incomplete history. Larger histories use the
agent's paginated run API or the retained archive. Each run preserves its
workload GVK and optional finish timestamp. Live logs require an
exact task UID; `TaskLogStreamer` allows a caller to preserve that incarnation
when reconnecting. The returned event channel closes at stream completion,
cancellation or a transport error. `AgentProxy` does not expose a separate
mid-stream error channel, so an early close alone cannot distinguish EOF from
transport failure.

## What an answer may build

The 4 MiB cap bounds the bytes of an answer, not what building it costs: an empty
element takes two bytes of protobuf or three of JSON on the wire, and about a
hundred or a few hundred bytes on the heap. So the proxy bounds what an answer
builds, before it builds it where it can. An answer over a bound fails the call with
an error that wraps `ErrDecode` and has the Kind of a decode error. It carries no
`ErrResponseTooLarge`, which stays for an answer over the size the proxy reads.
Whether a smaller page helps depends on the bound. It does against the budget of
elements in all (`ErrTooManyElements`), and the sync runner then asks again with half
the limit, as it does for `ErrResponseTooLarge`. It does not against more items than
the limit, an ID or a cursor over its bound; and a task with more conditions than the
cap is refused in any page that holds it, though the runner, which cannot tell it
from the budget, asks again smaller for it too, down to a page of that task alone.
Every call goes through these, the sync runner's listing and the REST listing of an
agent's tasks as well.

| What | Bound | Where it is checked | Error |
|---|---|---|---|
| Items of a `ListTasks` page | the request's limit; 1000, the most a page holds, when it names none | gRPC: on the wire, before decoding. HTTP: while decoding, before the item past the limit is decoded | `ErrTooManyItems` |
| Runs of a `ListTaskRuns` page | the page size of the request | as the items of a page | `ErrTooManyItems` |
| Elements of every repeated field and map of a gRPC answer, at any depth | 262144 (`1 << 18`) in all | on the wire, before decoding | `ErrTooManyElements` |
| Conditions of a task, in a listing, `GetTask` and the answer of `ApplyTask` | 64 (`MaxTaskConditions`) | HTTP: while decoding, before the 65th is decoded. gRPC: after decoding, which the budget above has capped | `ErrTooManyElements` |
| Task ID in a listing | 1024 bytes (`MaxTaskIDBytes`; the SDK limits a name to 63 bytes a segment) | after the item (HTTP) or the page (gRPC) is decoded | `ErrDecode` |
| Next cursor of a listing | 4096 bytes (`MaxCursorBytes`) | after the page is decoded | `ErrCursorTooLong` |

`ErrTooManyElements` is an `ErrTooManyItems` too; the sync runner tells the two apart:
it asks again with half the limit on the first, and ends the listing on the second.

Over gRPC every unary call (`ListTasks`, `GetTask`, `ApplyTask`, `DeleteTask`,
`CancelTask`, `ListTaskRuns`) is made with a codec of its own. Before
`proto.Unmarshal` it scans the wire bytes of the answer against the descriptor of the
response message, and counts every element of every repeated field and map in the
tree of messages under it: each entry of a map, each value of a packed scalar field.
A field the schema does not know, or whose wire type it does not fit, is skipped, as
`proto.Unmarshal` keeps it as unknown bytes. Messages nested deeper than protobuf
decodes (10000) are refused. The codec names no content-subtype, so the call sends
the same `application/grpc` as a call without it. The log stream needs no scan: its
messages hold no repeated field and no map.

Over HTTP every list the proxy builds from JSON is one of the three in the table:
they are decoded an element at a time by `jsonbounds.go`, and a key that comes twice
adds to the same list. The decoded structs hold no other list, and a field they do
not name is skipped without being built. HTTP has no budget in all, as the three
bounds add up to less.

What the tests measure (`TotalAlloc` of the process during one call, the fake agent
included; a call that reads a page of 4 MiB allocates 8 to 20 MiB by itself):

| Answer | Unbounded decode | Refused |
|---|---|---|
| `ListTasks`: a task with a million empty conditions, or env entries, or args; a thousand tasks with a thousand conditions each | 86 to 255 MiB (gRPC) | 2 to 4 MiB (gRPC) |
| `ListTasks`: a task with a million empty conditions | 499 MiB (HTTP) | 21 MiB (HTTP) |
| `GetTask`, `ApplyTask`: a task with a million empty conditions | 411 to 491 MiB (HTTP), 125 to 159 MiB (gRPC) | 13 MiB (HTTP), 4 MiB (gRPC) |
| `ListTaskRuns`: a page of a million empty runs | 748 MiB (HTTP), 604 MiB (gRPC) | 14 MiB (HTTP), 8 MiB (gRPC) |
| An answer of exactly 262144 elements | | decodes, 32 MiB |

The bounds refuse nothing compliant: the tests decode a page of a thousand tasks
with 64 conditions and 150 env entries each (215000 elements, 1.9 MB) over gRPC and a
thousand tasks with 64 conditions over HTTP. Within the 4 MiB the proxy reads, the
budget is reached only by an answer whose elements average under 16 bytes.

## Migration

Legacy subprocess `command`/`script` envelopes are normalized to `mode` when
inner fields already match the current schema. Legacy container command arrays
preserve arguments; an empty array retains the image default. Unknown fields
are rejected. Legacy script `wellKnown` and `custom.flag` runtimes require an
explicit migration: old inline-script flags cannot safely be inferred as the
new file-script interpreter arguments. The original saved configuration stays
available to the user for that choice.

Raft snapshot version 3 and the `SOLTI-CMD` command header/version 3 retain
structured capabilities and durable terminal execution records. Readers accept
compatible version-2 state; unversioned legacy command logs and version-1
snapshots remain unsupported. Startup validates retained snapshots and command
logs before replay, writable stores and TCP transport. Legacy unversioned logs
or version-1 snapshots fail with an actionable backup/offline-migration error.
There is no automatic legacy state migration; do not mix old and new replicas.

## Generating bindings

The authoritative schemas live in `soltiHQ/proto`. `task generate` vendors the
`agent`, `discover`, `task` and `raft` packages at the release pinned by
`proto_ref` in `Taskfile.yml` into `api/proto` and generates Go bindings into
`api/gen` with `buf generate`. The `protoc-gen-go` and `protoc-gen-go-grpc`
plugins are the `tool` versions from `go.mod`. Both directories are git-ignored.

The pool shares HTTP connections and holds one gRPC connection for each agent, to
the endpoint the agent advertises now. `Pool.Get` takes the agent ID: a `Get` for
another endpoint closes the agent's old connection, and so does one for an HTTP
endpoint, so the pool grows with the agents and not with the endpoints they once
advertised. `Pool.Forget(agentID)` closes the agent's connection for an agent that
is gone: the lifecycle runner calls it once it has deleted the agent. The pool
holds no token, `Get` takes it on every call, so the call after `Forget` presents
the token it is given. A call in flight on a connection that is closed fails.
`Close` releases them all.

`agentError` translates HTTP/gRPC status into the shared `errkind` boundary while
preserving operation and transport errors for `errors.Is`. Request deadlines come
from the caller context.

An agent chooses the text of its errors, so `AgentError.Error()`, the text of every
error the proxy returns for an agent's answer or for opening its log stream, is cut by
`CapAgentText` to 4 KiB (`MaxAgentTextBytes`) on a UTF-8 boundary and ended with `…`;
the chain behind it is whole. The sync runner puts words of its own before such an
error and stores it in the rollout and records it in the `sync_failed` event, so it
cuts the message once more: both hold at most 4 KiB and the marker. A log line that
carries such an error carries the cut text. An error the proxy makes itself (a bad
endpoint, a task name that does not match) holds no text of an agent. The helper
cuts nothing else: the archive of history keeps the error an agent reports for a
run under a bound of its own, `ExecutionErrorMaxBytes` (32 KiB), and refuses a
record over it instead of cutting it. An HTTP error body is read up to 4 KiB. A gRPC
client accepts a header list of 64 KiB at most (the status message travels in the
trailers); a status over that fails the call with an error of the transport, instead
of the 16 MiB that grpc-go allows by default.

## Endpoints and credentials

`Pool.Get` validates the endpoint again with `model.ValidateAgentEndpoint`, so
an agent stored before validation existed gets `ErrBadEndpointURL` instead of a
client. HTTP endpoints are `http(s)://host[:port][/base/path]` with no userinfo,
query or fragment. A reverse-proxy base path is absolute, has no empty, `.` or
`..` segments and must not need percent-encoding (`%2F`, `%2E` and the like are
rejected, not decoded). The task API path is appended to it after the trailing
`/` is trimmed: `https://gw.example.com/agents/a1/` calls
`https://gw.example.com/agents/a1/apis/solti.io/v1/tasks`.

gRPC endpoints are `host:port` without a path; the `http://`/`https://` prefix
and trailing `/` tolerated by `normalizeGrpcTarget` are also accepted, and TLS
still comes from `tls.client`. The pool dials `dns:///host:port`, so a host
named after a grpc-go resolver scheme (`unix:1`) is still a TCP host.

The stored bearer token is sent only when `agent_auth.require` is true. The
HTTP client does not follow redirects: a 3xx answer fails the call as
`ErrUnexpectedStatus`.
