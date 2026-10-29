# Package config

Package config aggregates per-package configuration structs and provides a
single `Default()` constructor for development use:

- **Config** — top-level struct embedding sub-configs from `httpserver`,
  `grpcserver`, `lifecycle`, `sync`, `server`, `wire` (auth), and `trigger`.
- **Default()** — returns safe development defaults. Zero-valued sub-configs
  inherit package-level defaults via each package's `withDefaults()`.

## Adding a new sub-config

1. Add a `Config` struct with `withDefaults()` in the owning package.
2. Add the field to `config.Config`.
3. Optionally set non-zero defaults in `Default()`.
4. Pass `cfg.<Field>` in the `internal/app` composition root.

## Loading from external sources

`Load()` reads in priority order: `Default()` → YAML file (`--config` / `CONFIG_PATH`)
→ env (`SOLTI_` prefix), then enforces `Config.Validate()`.

## TLS

`config.TLS` is one shared block applied to **all** transports (mirrors the SDK's
`solti-tls`). Both halves are opt-in — empty means plaintext.

- `tls.server` (`cert_file`, `key_file`, `client_ca_file`) — the CP's serving
  identity for the HTTP, HTTP-discovery and gRPC listeners. Setting
  `client_ca_file` turns on **mTLS** (client cert required).
- `tls.client` (`ca_file`, `cert_file`, `key_file`) — the CP as a client when
  dialing agents (proxy). `ca_file` verifies agent certs; `cert_file`/`key_file`
  present a client cert for mTLS.

```yaml
tls:
  server:
    cert_file: /etc/solti/tls/server.crt
    key_file:  /etc/solti/tls/server.key
    client_ca_file: /etc/solti/tls/agents-ca.crt   # optional → mTLS
  client:
    ca_file:   /etc/solti/tls/agents-ca.crt
    cert_file: /etc/solti/tls/cp-client.crt        # optional → mTLS
    key_file:  /etc/solti/tls/cp-client.key
```

Env equivalents: `SOLTI_TLS_SERVER_CERT_FILE`, `SOLTI_TLS_CLIENT_CA_FILE`, etc.

## Observed run history

```yaml
history:
  archive:
    retention: 168h
    max_records: 10000
    max_bytes: 33554432
  collector:
    tick_interval: 5s
    cycle_timeout: 8s
    max_concurrency: 2
    max_agents_per_cycle: 100
    page_size: 256
    max_pages: 4
    max_run_continuations: 1024
```

Environment equivalents include `SOLTI_HISTORY_ARCHIVE_RETENTION`,
`SOLTI_HISTORY_ARCHIVE_MAX_RECORDS`, `SOLTI_HISTORY_ARCHIVE_MAX_BYTES` and
`SOLTI_HISTORY_COLLECTOR_MAX_CONCURRENCY`. YAML and ENV use duration strings.
Zero values inherit owning-package defaults; negative values and collector counts
above their supported bounds fail configuration loading. `max_bytes` measures
conservative retained-record payload, not database/RSS size. Captures are best
effort and bound each task buffer to 8 MiB in addition to page/time limits.
