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
elements in all (`ErrTooManyElements`), and the sync runner then asks again with
half the limit, as it does for `ErrResponseTooLarge`. It does not against more items
than the limit, an ID or a cursor over its bound, nor against a task with more
conditions than the cap (`ErrTooManyConditions`): the page that holds that task is
refused whatever its size, so the sync runner does not ask again for it, and the
listing ends. Every call goes through these, the sync runner's listing and the REST
listing of an agent's tasks as well.

| What | Bound | Where it is checked | Error |
|---|---|---|---|
| Items of a `ListTasks` page | the request's limit; 1000, the most a page holds, when it names none | gRPC: on the wire, before decoding. HTTP: while decoding, before the item past the limit is decoded | `ErrTooManyItems` |
| Runs of a `ListTaskRuns` page | the page size of the request | as the items of a page | `ErrTooManyItems` |
| Elements of every repeated field and map of a gRPC answer, at any depth | 262144 (`1 << 18`) in all | on the wire, before decoding | `ErrTooManyElements` |
| Conditions of a task, in a listing, `GetTask` and the answer of `ApplyTask` | 64 (`MaxTaskConditions`) | HTTP: while decoding, before the 65th is decoded. gRPC: on the wire, at the 65th of a status message, before decoding; a status split in several occurrences of its field is refused after decoding | `ErrTooManyConditions` |
| Task ID in a listing | 1024 bytes (`MaxTaskIDBytes`; the SDK limits a name to 63 bytes a segment) | after the item (HTTP) or the page (gRPC) is decoded | `ErrDecode` |
| Next cursor of a listing | 4096 bytes (`MaxCursorBytes`) | after the page is decoded | `ErrCursorTooLong` |

`ErrTooManyElements` and `ErrTooManyConditions` are each an `ErrTooManyItems`
too, and the second an `ErrDecode` (the errors of the calls carry `ErrDecode`
either way), but neither is the other. The sync runner tells them apart: on
`ErrTooManyElements` it asks again with half the limit, down to a page of one
task; on `ErrTooManyConditions`, or on an `ErrTooManyItems` of neither kind (a
page over its limit), it ends the listing.

Over gRPC every unary call (`ListTasks`, `GetTask`, `ApplyTask`, `DeleteTask`,
`CancelTask`, `ListTaskRuns`) is made with a codec of its own. Before
`proto.Unmarshal` it scans the wire bytes of the answer against the descriptor of
the response message, and counts every element of every repeated field and map in
the tree of messages under it: each entry of a map, each value of a packed scalar
field. It counts the conditions of each task status as well, and refuses the answer
at the 65th in a status message, as `ErrTooManyConditions`, before the budget of
elements can trip: a task with a million conditions is one refusal over gRPC as it
is over HTTP, and the sync runner ends its listing at once. The cap is keyed by the
full name of the field, `solti.task.v1.TaskStatus.conditions`, and not by its
number: field 6 of the object metadata, the labels, has the same number and is not
capped. The scan counts a status by occurrence on the wire. A sender that splits one
status in several occurrences of the field gets by it, since `proto.Unmarshal`
merges them; the check after decoding refuses the merged task, at the cost of
decoding what the budget lets through (40 MiB in the test below, for 262144 empty
conditions). A field the schema does not know, or whose wire type it does not fit,
is skipped, as `proto.Unmarshal` keeps it as unknown bytes. Messages nested deeper
than protobuf decodes (10000) are refused. The codec names no content-subtype, so
the call sends the same `application/grpc` as a call without it. The log stream
needs no scan: its messages hold no repeated field and no map.

Over HTTP every list the proxy builds from JSON is one of the three in the table:
they are decoded an element at a time by `jsonbounds.go`, and a key that comes twice
adds to the same list. The decoded structs hold no other list, and a field they do
not name is skipped without being built. What is under a bound is built before the
call fails: the first `limit` items of a page, and the first 64 conditions of a task,
are decoded and dropped with the error; the element past the bound is not. HTTP has
no budget in all, as the three bounds add up to less.

What the committed tests send, and what a call allocated while it refused it
(`TotalAlloc` of the whole process during the call, the fake agent included, as
`go test -v` logs it; each test asserts under 64 MiB, and a call that reads a page of
4 MiB allocates 8 to 20 MiB by itself). The figures of the gRPC `ListTasks` floods
vary by a MiB between runs:

| Test | What it sends | Allocated |
|---|---|---|
| `TestGRPCListTasksRefusesNestedElementFloodBeforeDecode` | to `ListTasks` (limit 1000), over gRPC: one task with 1048576 empty conditions, or empty env entries, or empty args, or labels; 1000 tasks with 1000 empty conditions each; or 1000 tasks with 300 empty env entries each | 1 to 4 MiB |
| `TestHTTPListTasksRefusesConditionFlood` | to `ListTasks` (limit 1000), over HTTP: one task with 1048576 empty conditions | 21 MiB |
| `TestGetTaskRefusesNestedElementFlood` | to `GetTask`: one task with 1048576 empty conditions, over HTTP; the same, and one with 1048576 empty env entries, over gRPC | 13 MiB (HTTP), 4 MiB each (gRPC) |
| `TestApplyTaskRefusesNestedElementFlood` | to `ApplyTask`: one task with 1048576 empty conditions | 13 MiB (HTTP), 4 MiB (gRPC) |
| `TestAStatusSplitUpToTheBudgetIsRefusedAfterDecoding` | to `GetTask`, over gRPC: one task whose status is split in 4096 occurrences of 64 empty conditions, 262144 in all, which the scan lets through; it is refused after it is decoded | 40 MiB |
| `TestListTaskRunsRefusesExtraRunsBeforeDecode` | a `ListTaskRuns` page (limit 1000) of 1048576 empty runs over HTTP, of 2097152 (4 MiB) over gRPC | 14 MiB (HTTP), 8 MiB (gRPC) |
| `TestCompliantAnswersNearTheBudgetStillDecode` | to `GetTask`, over gRPC: one task with exactly 262144 empty env entries; it decodes | 32 MiB |

Before these bounds the floods above allocated 86 to 748 MiB a call, all but the
million labels, which the code before them accepted at 2 MiB (Go keeps one map entry
for equal keys). That was measured when the bounds were written, by running the tests
that existed then against that code. The two rows added since (a thousand tasks with
300 empty env entries each, and the split status) were not run against it. No
committed test reproduces it, since they assert and log the refused side only.

What the bounds let through is what stays within the numbers above, and nothing more
is promised: the numbers are the control plane's choice, and no test shows that every
agent's answers stay within them. The tests decode a page of 1000 tasks with 64
conditions and 150 env entries each over gRPC (215000 elements, 1.87 MB), a page of
1000 tasks with 64 conditions each over HTTP, and an answer of exactly 262144
elements, and refuse one element more. Within the 4 MiB the proxy reads, the budget
is reached only by an answer whose elements average under 16 bytes; a listing that
hits it only through its page size is asked again smaller by the sync runner.

## Migration

Legacy subprocess `command`/`script` envelopes are normalized to `mode` when
inner fields already match the current schema. Legacy container command arrays
preserve arguments; an empty array retains the image default. Unknown fields
are rejected. Legacy script `wellKnown` and `custom.flag` runtimes require an
explicit migration: old inline-script flags cannot safely be inferred as the
new file-script interpreter arguments. The original saved configuration stays
available to the user for that choice.

Raft snapshot version 4 and the `SOLTI-CMD` command header/version 4 retain
structured capabilities, durable terminal execution records and derived agent
credentials. Readers accept version-3 state, which has no derived credential;
version-2 state, unversioned legacy command logs and version-1 snapshots remain
unsupported. Startup validates retained snapshots and command
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
is gone or whose credential was reset: the lifecycle runner calls it once it has
deleted the agent, and so does the API once the delete of an agent, or the reset
of its credential, is committed. The pool holds no token, `Get` takes it on every
call, so the call after `Forget` presents the token it is given. A call in flight
on a connection that is closed fails.
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

The bearer token is sent only when `agent_auth.require` is true. The callers
take it from the agent service (`agent.Service.AgentToken`): the token of a
stored credential (an agent that enrolled without the enrollment secret, or one
an older version enrolled), or the token derived from the secret for an agent
enrolled under it, which no store holds. An agent whose
derived credential podium cannot use, with no secret set or of another secret,
gets no call: the token lookup fails first. The HTTP client does not follow
redirects: a 3xx answer fails the call as `ErrUnexpectedStatus`.
