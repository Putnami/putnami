# Putnami Cloud runtime for Go

`go.putnami.dev/cloud/runtime` connects a Go workload that runs on Putnami Cloud
to the platform's configuration service. It adds remote config and secrets
sources to the [`go.putnami.dev/config`](../../../go/framework/config/README.md)
loader, and signs their requests with the workload identity that the GCP
metadata server issues.

| Package | Import path |
|---|---|
| Sources, discovery and token sources | `go.putnami.dev/cloud/runtime` |
| Activation | `go.putnami.dev/cloud/runtime/activate` |

## Support status

- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../../putnami.support.json).
- **Owner**: `go.putnami.dev/cloud/runtime` (`cloud/runtime/go`).
- **What may still change**: the exported names and the activation import.
  Stable support requires a version gate on the public surface and shared
  fixtures that prove parity with the TypeScript runtime destination.
- **Evidence**: the package tests, which cover discovery, the three sources,
  retries, cancellation, the snapshot fallback and the token sources.

## Activation

Blank-import the `activate` package from the workload's `main` package:

```go
import (
    "go.putnami.dev/config"

    _ "go.putnami.dev/cloud/runtime/activate"
)

var serverDef = config.Config[ServerConfig]("server")

func main() {
    server, err := config.Load(serverDef)
    // ...
}
```

The import registers three source discoverers with
`config.RegisterSourceDiscoverer` from `init()`, so they exist before the first
`config.Load`. Without it, `config.Load` reads only local YAML, `CONFIG_DATA` and
`env` tags. Importing the root package alone, for its types or constructors,
registers nothing.

## Sources

| Source | Priority | Enabled when | Request |
|---|---|---|---|
| `config-server` | 50 | `CONFIG_SERVER_URL` is set | `POST <CONFIG_SERVER_URL>/api/configs/resolve`, or `GET` on `CONFIG_SERVER_URL` as-is when it already ends in `/api/configs/resolve` |
| `secrets-server` | 55 | `CONFIG_SERVER_URL` is set and is not an exact `/api/configs/resolve` URL | `POST <CONFIG_SERVER_URL>/api/secrets/resolve` |
| `prepared-config-boot` | 50 | `PUTNAMI_CONFIG_BOOT_BINDING` is set | `POST <origin of CONFIG_SERVER_URL>/internal/config/prepared-configuration/boot` |

The loader's local files sit below these sources and `CONFIG_DATA` (60) above
them, so `CONFIG_DATA` stays the local emergency override and an `env` tag
still wins over every source.

When `PUTNAMI_CONFIG_BOOT_BINDING` is set, the prepared boot source is the only
remote source: `config-server` and `secrets-server` stay off.

## Environment

| Variable | Read by | Meaning |
|---|---|---|
| `CONFIG_SERVER_URL` | all sources | Config service base URL, or an exact `/api/configs/resolve` URL whose query is kept verbatim. |
| `CONFIG_SERVER_AUDIENCE` | all sources | Audience of the workload identity token. Defaults to `CONFIG_SERVER_URL`. Required by the prepared boot source. On a `404`, `config-server` retries the same request once against this origin. |
| `PUTNAMI_CLOUD_TOKEN`, `CONFIG_SERVER_TOKEN`, `PUTNAMI_TOKEN` | all sources | Static bearer token. The first non-empty one, in this order, wins over workload identity. |
| `K_SERVICE`, `GOOGLE_CLOUD_PROJECT` | all sources | Either one marks the process as running on GCP. Without them, a 200 ms TCP probe to `metadata.google.internal:80` decides. On GCP, tokens come from the metadata server. |
| `CONFIG_SERVER_TIMEOUT` | all sources | Timeout of one attempt, as a Go duration. Default `5s`. |
| `CONFIG_SERVER_RETRY_BUDGET` | all sources | Total retry time of a required source. Default `30s`. `0` turns retries off. |
| `CONFIG_SERVER_REQUIRED` | `config-server`, `secrets-server` | `1`, `true`, `yes` or `on` makes a failure stop startup. When unset, a source is required if `K_SERVICE` is set, `NODE_ENV` is `production` or `prod`, or `APP_ENV` is `prod` or `production`. |
| `APP_NAME` | `config-server`, `secrets-server` | Application name sent in the resolve request. |
| `APP_ENV` | `config-server`, `secrets-server` | Environment sent in the resolve request. Default `local`. |
| `CONFIG_VERSION` | `config-server` | Revision pin. When set, the request carries `version` and `pinned=true`, and the service resolves only that frozen revision. |
| `APP_VERSION` | `secrets-server` | Version sent in the secrets resolve request. It never pins config. |
| `SCHEMA_HASH` | `config-server`, `secrets-server` | Optional schema hash. A mismatch is logged as a warning. |
| `CONFIG_SNAPSHOT_URI` | `config-server` | `gs://<bucket>/<object>` of the encrypted last-known-good config snapshot. |
| `CONFIG_SNAPSHOT_KMS_KEY` | `config-server` | Cloud KMS key that decrypts the snapshot. |
| `PUTNAMI_CONFIG_BOOT_BINDING` | `prepared-config-boot` | Opaque `pcb_` reference of the prepared configuration. |

## Contract

- **One fetch per process.** Each source loads once and shares the result with
  every `config.Load` call, while the process environment does not change.
- **Cancellation.** Every source implements `config.ContextSource`.
  `config.LoadContext` stops requests, retry waits and the snapshot fallback
  when its context ends, and returns an error that wraps `ctx.Err()`.
- **Retries.** A required source retries network errors and the statuses `408`,
  `425`, `429` and `5xx`, with backoff from 100 ms to 2 s, inside
  `CONFIG_SERVER_RETRY_BUDGET`. Other statuses fail at once.
- **Optional sources.** A source that is not required logs a warning on failure
  and returns no values, so local config applies.
- **Snapshot fallback.** When `CONFIG_SNAPSHOT_URI` is set and the service stays
  unreachable for the whole retry budget, `config-server` reads the snapshot
  from Cloud Storage, decrypts it with Cloud KMS, and logs a warning that the
  workload runs on stale config. A `401`, `403`, `404` or an unresolved answer
  never falls back: the workload is no longer entitled to that config.
- **Prepared boot.** The boot source sends its request only to the origin of
  `CONFIG_SERVER_URL` over HTTPS (plain HTTP only to a loopback host), refuses
  redirects, rejects a response over 4 MiB or with unknown fields, and never
  falls back to a snapshot.
- **Transport.** A non-HTTPS `CONFIG_SERVER_URL` to a host other than loopback
  logs a warning, because the token and the resolved values travel in clear.
- **Identity.** `GcpMetadataTokenSource` caches the ID token until 5 minutes
  before its `exp` claim, or for 50 minutes when the token carries none.
- **No credentials.** When no token is configured and the process is not on GCP,
  `config-server` and `secrets-server` send their requests without an
  `Authorization` header and the service decides. The prepared boot source
  never sends a request without a token: it fails.
