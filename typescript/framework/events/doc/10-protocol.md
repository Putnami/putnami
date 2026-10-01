# Events Protocol

The canonical Putnami events protocol lives in:

```text
protocols/events
```

It defines the shared event envelope, Event Server stream frames, handler option
vocabulary, Event Server discovery profile, JSON schemas, fixtures, a
non-normative gRPC proto, conformance runner manifest, and Go package:

```go
import events "go.putnami.dev/protocol/events"
```

TypeScript exposes the matching constants and types from:

```typescript
import {
  PUTNAMI_EVENTS_PROTOCOL,
  EVENT_FRAME_TYPES,
  EVENT_SERVER_DISCOVERY_PATH,
  MANAGED_PUBLISH_CONTRACT_VERSION,
  isCompatibleEventServer,
  isManagedPublishFrame,
  type EventEnvelope,
  type EventServerFrame,
  type EventServerCapabilities,
} from '@putnami/events/protocol';
```

## Managed workload publishing

Managed workloads use the stricter `EventServerFrame` publish profile defined
by `protocols/events/doc/12-managed-publish.md`. It stays on
`putnami.events.v1`; `MANAGED_PUBLISH_CONTRACT_VERSION` is the separate managed
admission/config compatibility gate.

The canonical request and outcome corpus lives under
`protocols/events/fixtures/managed-publish/v1`. TypeScript conformance tests
load those files directly, while Go and cloud consumers can load the same bytes
from the embedded protocol filesystem. Do not create a TypeScript-owned fixture
copy. Managed retries preserve exact request bytes and the Event Server route;
they never automatically dual-publish or fall back to direct Pub/Sub.

The protocol identifier is:

```text
putnami.events.v1
```

The gRPC endpoint profile is illustrated by a non-normative protobuf file
(reference only; the JSON schemas and fixtures are canonical):

```text
protocols/events/proto/putnami/events/v1/event_server.proto
```

## Canonical Envelope Field Names

Use these names at protocol boundaries:

| Field | Description |
|-------|-------------|
| `protocol` | Optional on internal envelopes, required on Event Server frames. |
| `id` | Stable event message id. |
| `topic` | Event topic name. |
| `channel` | Logical routing channel. |
| `payload` | Event payload. |
| `key` | Routing or partition key. |
| `dedupeKey` | Idempotency key. |
| `topicVersion` | Topic contract version. |
| `timestamp` | RFC3339 timestamp. |
| `attributes` | String metadata. |
| `attempt` | Delivery attempt, starting at `1`. |
| `traceId` | Distributed tracing correlation id. |

Avoid implementation-specific aliases such as `topicChannel`; the canonical
wire field is `channel`.

## Compatibility Rule

Framework implementations, Putnami Event Plane, and broker adapters should validate
against the same fixtures in `protocols/events/fixtures`. Changes that
alter frame or envelope shape should update the protocol package first, then the
TypeScript and Go implementations.

## Event Server Compatibility

Compatible event servers expose:

```text
GET /.well-known/putnami/events
```

The response is `EventServerCapabilities`. Clients can validate that the
server speaks `putnami.events.v1`, supports the required endpoint profile, and
advertises required features before connecting:

```typescript
if (!isCompatibleEventServer(capabilities, { transports: ['sse'], replay: true })) {
  throw new Error('Event Server is not compatible with this client.');
}
```

The protocol defines endpoint profiles for SSE, WebSocket, HTTP publish, and
gRPC. Each profile maps back to the same canonical Event Server frames and
envelope fields, so a server can proxy Redis, Pub/Sub, Postgres, or Putnami
Event Plane while remaining client-compatible.

## Conformance

The public conformance suite is described by:

```text
protocols/events/conformance/manifest.json
```

It is designed as a black-box runner against a deployed Event Server. This lets
Putnami Cloud keep the server implementation private while still enforcing that
SSE, WebSocket, HTTP publish, gRPC, discovery, errors, replay, limits, and
payload semantics remain compatible with open clients.

Auth auto-wiring is not part of this runner. The runner consumes already
resolved credentials from CI or local env; managed identity wiring belongs in a
dedicated deployment/auth integration.

## Putnami Event Plane

Putnami Event Plane is the managed Event Server implementation. It is not the
only compatible implementation; it is the simplest managed path when an
application wants every Event Server feature without operating the underlying
providers directly.

For server-to-server calls into Putnami Event Plane, auth should be secure by
default and auto-wired from deployment identity. The normal managed path should
not require application code like `token: async () => ...`; the deploy/runtime
layer should resolve `workloadIdentity` or `serviceAccount` credentials and
attach the correct auth to Event Server calls. Explicit bearer tokens, custom
headers, and custom token providers remain escape hatches for self-hosted or
non-managed environments.

Browser and mobile clients are different: they forward user-scoped credentials
with `eventClient({ token, headers })`, usually a refreshed bearer token plus
optional device/tenant headers.
