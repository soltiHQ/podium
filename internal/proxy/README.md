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
limit up to 1000. Offset pagination is unsupported. The live run-history
inspector joins only pages with the same task UID and resource version. It
rejects repeated cursors and histories exceeding 1024 rows, 16 pages or 8 MiB
of logical payload, returning an actionable error instead of an incomplete
history. Larger histories use the agent's paginated run API or the retained
archive. Each run preserves its workload GVK and optional finish timestamp. Live logs require an
exact task UID; `TaskLogStreamer` allows a caller to preserve that incarnation
when reconnecting. The returned event channel closes at stream completion,
cancellation or a transport error. `AgentProxy` does not expose a separate
mid-stream error channel, so an early close alone cannot distinguish EOF from
transport failure.

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

The pool shares HTTP connections and caches gRPC connections per endpoint.
`Close` releases both. `agentError` translates HTTP/gRPC status into the shared
`errkind` boundary while preserving operation and transport errors for
`errors.Is`. Request deadlines come from the caller context.

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
