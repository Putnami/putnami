# @putnami/cloud

The Putnami Cloud runtime destination for TypeScript workloads. It gives
`@putnami/runtime` two remote config sources (config and secrets) and gives
`@putnami/events` a managed Event Server destination. Both authenticate with
an operator token or with Google workload identity.

## Import

Add `@putnami/cloud` to the workload's `dependencies` in `package.json`, then
run `putnami deps install`.

| Entry point | Contents |
| --- | --- |
| `@putnami/cloud/runtime` | `register`, the config and secrets sources, the token sources, `eventServerDestination` |
| `@putnami/cloud/runtime/testing` | `setSyncFetchForTest` and `resetSyncFetchForTest`, which swap the HTTP transport in tests |

The testing entry point is separate so that production code cannot replace the
transport that the config and secrets calls use.

## Activation

`register` adds the two config sources to the framework config loader:

```typescript
// src/activate.ts
import { register } from '@putnami/cloud/runtime';
import { registerConfigLoaderResetHook, registerSourceDiscoverer } from '@putnami/runtime';

register({ registerSourceDiscoverer, registerConfigLoaderResetHook });
```

Import `./activate` first in `src/main.ts`. The framework also calls
`register` itself when it can `require('@putnami/cloud/runtime')`, which works
when `node_modules` exist (`putnami serve`, tests). A bun-compiled binary
cannot resolve that `require`, so a deployed workload needs the static import.
Registering twice is safe: the loader dedupes discoverers.

The Event Server destination has no activation step. Call
`eventServerDestination(config.events)` and pass the returned transport to
`@putnami/events`.

## Environment

The config and secrets sources read the environment each time the loader
discovers sources. Nothing is read at import time.

| Variable | Effect |
| --- | --- |
| `CONFIG_SERVER_URL` | Turns both sources on. A base URL sends `POST <url>/api/configs/resolve` and `POST <url>/api/secrets/resolve`. A URL that ends in `/api/configs/resolve` is fetched as-is with `GET`, and the secrets source stays off. Empty, `undefined` and `null` mean unset. |
| `PUTNAMI_CLOUD_TOKEN`, `CONFIG_SERVER_TOKEN`, `PUTNAMI_TOKEN` | Bearer token, first set name wins. |
| `K_SERVICE`, `GOOGLE_CLOUD_PROJECT` | With no token set, either one selects a Google metadata ID token. |
| `CONFIG_SERVER_AUDIENCE` | Audience of that ID token (default: `CONFIG_SERVER_URL`). On a `404`, the config request is retried once against this origin. |
| `APP_NAME` | Application name sent to both endpoints. |
| `APP_ENV` | Environment sent to both endpoints, through `getEnv()` from `@putnami/runtime`. |
| `CONFIG_VERSION` | Pins the config resolve to that frozen version (`pinned: true`). |
| `APP_VERSION` | Version sent to the secrets endpoint. It never pins config. |
| `SCHEMA_HASH` | Schema hash sent to both endpoints. |
| `CONFIG_SERVER_TIMEOUT` | Per-request timeout, such as `1500ms` or `2s`. Default `5s`. |
| `CONFIG_SERVER_RETRY_BUDGET` | Total retry time for a required source. Default `30s`. `0` turns retries off. |
| `CONFIG_SERVER_REQUIRED` | `1`, `true`, `yes` or `on` make a failure stop startup. Unset, a source is required when `K_SERVICE` is set or `NODE_ENV` or `APP_ENV` is `production` or `prod`. |
| `CONFIG_SNAPSHOT_URI`, `CONFIG_SNAPSHOT_KMS_KEY` | `gs://<bucket>/<object>` of an encrypted last-known-good config, and the Cloud KMS key that decrypts it. |

## Contract

- **Priority.** The config source has priority 50 and the secrets source 55.
  Both sit above the local `.secrets` file (35) and below `CONFIG_DATA` (60)
  and field-level `Env(...)` values (80).
- **Failure.** An optional source logs a warning and returns nothing, so local
  sources apply. A required source retries `408`, `425`, `429`, `5xx` and
  network errors with backoff from 100 ms to 2 s, inside the retry budget,
  then throws `RemoteConfigError`. Other statuses fail at once.
- **Snapshot.** When a required config source exhausts its budget on a
  retryable failure and a snapshot is configured, the source boots on the
  snapshot and logs a warning. A `401`, `403`, `404` or `resolved: false` never
  falls back to the snapshot.
- **Cache.** One resolve per distinct request per process. Once registered,
  `resetConfigLoader()` from `@putnami/runtime` clears these caches.
- **Event Server.** `eventServerDestination` accepts only
  `transport: 'eventserver'`, contract version 1 and protocol
  `putnami.events.v1`, with HTTPS origins for `endpoint` and `audience`. It
  pins the topology generation and refuses a stale route before it asks for a
  token. Tokens are JWTs whose `aud` must equal the audience. They are cached
  per audience until one minute before expiry, with one shared refresh.
  Errors never carry token or provider text.

## Support status

- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../../putnami.support.json).
- **Owner**: `@putnami/cloud` (`cloud/runtime/typescript`).
- **What may still change**: the exported names and the activation call.
  Stable support requires a version gate on the public surface and shared
  fixtures that prove parity with the Go runtime destination.
- **Evidence**: the tests under `test/`, and the `sites/putnami.dev` workload,
  which activates `@putnami/cloud/runtime`.

## Hosts

Outside the configured URLs, the package calls Google endpoints only:

| Host | Use |
| --- | --- |
| `metadata.google.internal` | ID tokens for the config server and the Event Server, and the access token for the snapshot |
| `storage.googleapis.com` | Snapshot download |
| `cloudkms.googleapis.com` | Snapshot decryption |
