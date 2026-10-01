# Adapters And Config

`@putnami/document` chooses an adapter from typed runtime config. The repository reads only the store config it needs, which keeps named stores and future adapters predictable.

## Config shape

Default store:

```yaml
document:
  backend: memory
  strictIndexes: false
  slowOperationThresholdMs: 0
```

Firestore default store:

```yaml
document:
  backend: firestore
  projectId: my-project
  databaseId: (default)
  emulatorHost: 127.0.0.1:8080
  credentials: ${FIRESTORE_CREDENTIALS}
```

Named stores:

```yaml
document:
  backend: memory

  analytics:
    backend: firestore
    projectId: analytics-project

  audit:
    backend: memory
    strictIndexes: true
```

Collection definitions opt into a named store with `db`:

```ts
Collection(
  'audit',
  {
    id: DocumentId(String),
    action: Field(String),
  },
  { db: 'audit' },
);
```

## Config fields

- `backend`: currently `memory` or `firestore`
- `projectId`: Firestore project id
- `databaseId`: Firestore database id, default `(default)`
- `emulatorHost`: Firestore emulator host
- `credentials`: Firestore credentials JSON or key file path
- `strictIndexes`: repository-level declared index enforcement. Defaults to `true` for the `firestore` backend (uncovered queries would otherwise fail only at runtime with `FAILED_PRECONDITION`) and `false` for `memory`; set explicitly to override.
- `slowOperationThresholdMs`: warn when operations exceed this duration

## Memory adapter

Use the memory adapter for:

- unit tests
- lightweight local development
- applications that do not need an external document service

Behavior:

- strong and eventual reads are both available
- transactions are supported
- composite document ids are supported
- filters are evaluated in process
- cursors are opaque base64url tokens over the sorted result set

## Firestore adapter

Use the Firestore adapter when you want a managed remote document backend with the same repository API.

Behavior:

- loaded lazily through `import('@google-cloud/firestore')`
- requires the optional peer dependency
- supports transactions and batch writes
- supports cursor pagination
- currently requires a single scalar document id

The adapter throws `DocumentError` with `ADAPTER_NOT_INSTALLED` when the peer dependency is missing.

## Consistency

Read methods accept `consistency: 'strong' | 'eventual'`.

Current adapters accept both values. Future adapters can reject unsupported strong reads with `StrongConsistencyUnsupported`.

## Backend lifecycle

Backends are cached and ref-counted per resolved config.

```ts
import { closeAllBackends, closeBackend, useBackend } from '@putnami/document';

const defaultBackend = await useBackend();
const auditBackend = await useBackend('audit');

await closeBackend('audit');
await closeAllBackends();
```

The `document()` plugin calls `closeAllBackends()` during application shutdown.

## Observability

Repositories record counters, histograms, and structured logs per operation:

- `document.get.<collection>`
- `document.find.<collection>`
- `document.save.<collection>`
- `document.delete.<collection>`
- `document.operation.duration`
- `document.operation.error`
- `document.operation.slow`
- `document.backend.count`

Slow-operation warnings use `document.slowOperationThresholdMs`.
